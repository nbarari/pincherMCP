"""Pincher Python AST extractor (embedded via //go:embed).

Reads source from stdin, takes <relpath> as argv[1]. Emits a single JSON
object on stdout:

    {"symbols": [...], "edges": [...], "module": "..."}

or, on SyntaxError:

    {"error": "..."}

Exits 0 in both cases so the Go caller distinguishes parse failure from
process failure by inspecting the JSON. Pure stdlib, Python 3.8+.
"""

import ast
import json
import sys


def build_line_offsets(source):
    """Byte offset of each 1-indexed line. Slot 0 unused; slot 1 = 0."""
    offsets = [0, 0]
    for i, b in enumerate(source):
        if b == 0x0A:
            offsets.append(i + 1)
    return offsets


def byte_offset(line_offsets, lineno, col_offset):
    if lineno < 1 or lineno >= len(line_offsets):
        return 0
    return line_offsets[lineno] + (col_offset or 0)


def module_qn(relpath):
    """Match moduleQN() in extractor.go: strip extension, slashes → dots."""
    base = relpath
    dot = base.rfind(".")
    if dot > 0:
        base = base[:dot]
    return base.replace("/", ".").replace("\\", ".")


def is_test_name(name):
    return name.startswith("test_") or name.startswith("Test")


def collect_dunder_all(tree):
    """Module-level `__all__ = [...]`: return the set of names, or None."""
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        for target in node.targets:
            if isinstance(target, ast.Name) and target.id == "__all__":
                names = _literal_string_seq(node.value)
                if names is not None:
                    return names
    return None


def _literal_string_seq(value):
    if not isinstance(value, (ast.List, ast.Tuple)):
        return None
    names = set()
    for elt in value.elts:
        if isinstance(elt, ast.Constant) and isinstance(elt.value, str):
            names.add(elt.value)
        else:
            return None
    return names


def safe_unparse(node):
    """ast.unparse on 3.9+; empty string on older Python."""
    if node is None:
        return ""
    try:
        return ast.unparse(node)
    except (AttributeError, NotImplementedError):
        return ""


def build_signature(node):
    """Reconstruct the def/class line with decorators, async, annotations."""
    lines = []
    for dec in getattr(node, "decorator_list", []) or []:
        lines.append("@" + safe_unparse(dec))
    if isinstance(node, ast.ClassDef):
        parts = [safe_unparse(b) for b in node.bases]
        parts += [
            (kw.arg + "=" if kw.arg else "**") + safe_unparse(kw.value)
            for kw in node.keywords
        ]
        suffix = "(" + ", ".join(parts) + ")" if parts else ""
        lines.append("class " + node.name + suffix + ":")
    else:
        prefix = "async def" if isinstance(node, ast.AsyncFunctionDef) else "def"
        args = safe_unparse(node.args) if node.args else ""
        ret = ""
        if node.returns is not None:
            ret = " -> " + safe_unparse(node.returns)
        lines.append(prefix + " " + node.name + "(" + args + ")" + ret + ":")
    return "\n".join(lines)


def collect(node, parent_qn, dunder_all, line_offsets, source_len,
            in_class, class_qn, imports_map, symbols, edges):
    """Walk child nodes; emit one record per FunctionDef/AsyncFunctionDef/ClassDef.

    Function/method bodies are scanned for ast.Call nodes; each call becomes
    a CALLS edge with from_qn set to the enclosing function's QN. class_qn
    is the QN of the immediately-enclosing class (or "" outside any class),
    used to rewrite `self.X` callers into resolvable absolute paths.
    """
    for child in ast.iter_child_nodes(node):
        if not isinstance(
            child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)
        ):
            continue
        name = child.name
        qn = parent_qn + "." + name if parent_qn else name

        if isinstance(child, ast.ClassDef):
            kind = "Class"
        elif in_class:
            kind = "Method"
        else:
            kind = "Function"

        start_line = child.lineno
        # Account for decorator lines so StartByte points at the first @decorator.
        if getattr(child, "decorator_list", None):
            first_dec = child.decorator_list[0]
            start_line = min(start_line, first_dec.lineno)
        start_byte = byte_offset(line_offsets, start_line, 0)

        end_line = getattr(child, "end_lineno", None) or start_line
        end_col = getattr(child, "end_col_offset", 0) or 0
        end_byte = byte_offset(line_offsets, end_line, end_col)
        if end_byte > source_len:
            end_byte = source_len

        if dunder_all is not None:
            is_exp = name in dunder_all
        else:
            is_exp = not name.startswith("_")

        symbols.append({
            "name": name,
            "qualified_name": qn,
            "kind": kind,
            "parent": parent_qn if kind == "Method" else "",
            "signature": build_signature(child),
            "docstring": ast.get_docstring(child, clean=True) or "",
            "is_exported": is_exp,
            "is_test": is_test_name(name),
            "start_byte": start_byte,
            "end_byte": end_byte,
            "start_line": start_line,
            "end_line": end_line,
        })

        # Walk this function/method body for calls, but skip nested scopes
        # (they're visited by the recursive collect() call below and emit
        # their own from_qn-anchored edges).
        if not isinstance(child, ast.ClassDef):
            collect_calls_in_body(child, qn, class_qn, imports_map, edges)

        next_class_qn = qn if isinstance(child, ast.ClassDef) else class_qn
        collect(
            child, qn, dunder_all, line_offsets, source_len,
            in_class=isinstance(child, ast.ClassDef),
            class_qn=next_class_qn,
            imports_map=imports_map,
            symbols=symbols, edges=edges,
        )


def collect_calls_in_body(fn_node, from_qn, class_qn, imports_map, edges):
    """Emit one CALLS edge per ast.Call inside fn_node, stopping at nested defs.

    Nested FunctionDef / AsyncFunctionDef / ClassDef boundaries are NOT
    descended into here — collect()'s outer recursion will visit them and
    record their own calls under their own from_qn.
    """
    stack = list(fn_node.body)
    while stack:
        node = stack.pop()
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            continue
        if isinstance(node, ast.Call):
            to_name = call_target_name(node.func, class_qn, imports_map)
            if to_name:
                edges.append({
                    "from_qn": from_qn,
                    "to_name": to_name,
                    "kind": "CALLS",
                    "confidence": 0.7,
                })
        for sub in ast.iter_child_nodes(node):
            stack.append(sub)


def call_target_name(func, class_qn, imports_map):
    """Render a Call's func expression as a dotted ToName, or '' if not statically resolvable."""
    if isinstance(func, ast.Name):
        return imports_map.get(func.id, func.id)
    if isinstance(func, ast.Attribute):
        return attribute_chain_name(func, class_qn, imports_map)
    return ""


def attribute_chain_name(node, class_qn, imports_map):
    """Walk an attribute chain back to its base Name and produce a dotted path.

    Rewrites `self.X` → `<class_qn>.X` when we're inside a method so the
    resolver can match it against the method's actual QN. Returns '' when
    the base isn't a plain Name (e.g. `func().attr` — needs type info we
    don't have).
    """
    parts = []
    cur = node
    while isinstance(cur, ast.Attribute):
        parts.append(cur.attr)
        cur = cur.value
    if not isinstance(cur, ast.Name):
        return ""
    parts.reverse()
    base = cur.id
    if base == "self" and class_qn:
        return class_qn + "." + ".".join(parts)
    mapped = imports_map.get(base, base)
    return mapped + "." + ".".join(parts)


def build_imports_map(tree):
    """Map each locally-bound import name to its full dotted path.

    Used by the CALLS pass to rewrite alias-resolved call expressions:
      from foo import bar as b   →  b()       → foo.bar
      from .sib import x         →  x()       → .sib.x   (relative kept)
      import requests            →  requests  → requests (identity)
      import os.path as op       →  op.exists → os.path.exists
    """
    out = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            for alias in node.names:
                if alias.asname:
                    out[alias.asname] = alias.name
                else:
                    # `import x.y.z` binds local name `x`; references like
                    # `x.y.z.foo` are written with the full path inline,
                    # so map the local name to itself.
                    first = alias.name.split(".")[0]
                    out[first] = first
        elif isinstance(node, ast.ImportFrom):
            base = node.module or ""
            if node.level:
                base = ("." * node.level) + base
            for alias in node.names:
                local = alias.asname or alias.name
                full = base + "." + alias.name if base else alias.name
                out[local] = full
    return out


def collect_imports(tree, module):
    edges = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                edges.append({
                    "from_qn": module,
                    "to_name": alias.name,
                    "kind": "IMPORTS",
                    "confidence": 1.0,
                })
        elif isinstance(node, ast.ImportFrom):
            base = node.module or ""
            if node.level:
                base = ("." * node.level) + base
            for alias in node.names:
                target = base + "." + alias.name if base else alias.name
                edges.append({
                    "from_qn": module,
                    "to_name": target,
                    "kind": "IMPORTS",
                    "confidence": 1.0,
                })
    return edges


def main():
    relpath = sys.argv[1] if len(sys.argv) > 1 else ""
    module = module_qn(relpath)
    source = sys.stdin.buffer.read()

    try:
        tree = ast.parse(source)
    except SyntaxError as e:
        json.dump({"error": str(e)}, sys.stdout)
        return

    line_offsets = build_line_offsets(source)
    dunder_all = collect_dunder_all(tree)
    imports_map = build_imports_map(tree)
    symbols = []
    edges = []

    # Emit one Module symbol per file so IMPORTS edges have a stable
    # endpoint on both sides (matches the Go extractor's convention at
    # extractor.go:432-448). Without this, every Python IMPORTS edge
    # would lack a resolvable from-side and stay in pending_edges.
    last_line = max(1, len(line_offsets) - 1)
    short_name = module.rsplit(".", 1)[-1] if module else ""
    symbols.append({
        "name": short_name,
        "qualified_name": module,
        "kind": "Module",
        "parent": "",
        "signature": "",
        "docstring": ast.get_docstring(tree, clean=True) or "",
        "is_exported": True,
        "is_test": False,
        "start_byte": 0,
        "end_byte": len(source),
        "start_line": 1,
        "end_line": last_line,
    })

    collect(
        tree, parent_qn=module, dunder_all=dunder_all,
        line_offsets=line_offsets, source_len=len(source),
        in_class=False, class_qn="", imports_map=imports_map,
        symbols=symbols, edges=edges,
    )
    edges.extend(collect_imports(tree, module))

    json.dump(
        {"symbols": symbols, "edges": edges, "module": module},
        sys.stdout,
    )


if __name__ == "__main__":
    main()
