"""Minimal example: call `pincher search` from Python via the generated SDK.

Assumes you've run `scripts/generate-sdks.sh python` against a running
pincher, and that you've `pip install -e sdks/python/generated` so the
package is importable.

Run with:
    PINCHER_HTTP_URL=http://localhost:8080 \\
    PINCHER_HTTP_KEY=$(cat ~/.pincher-key) \\
    python sdks/python/examples/search.py ProcessPayment
"""

from __future__ import annotations

import os
import sys

from pincher_sdk import ApiClient, Configuration
from pincher_sdk.api.search_api import SearchApi
from pincher_sdk.models.search_request import SearchRequest


def main() -> int:
    base_url = os.environ.get("PINCHER_HTTP_URL", "http://localhost:8080")
    api_key = os.environ.get("PINCHER_HTTP_KEY")
    query = sys.argv[1] if len(sys.argv) > 1 else "Hello"

    cfg = Configuration(host=base_url)
    if api_key:
        cfg.access_token = api_key

    with ApiClient(cfg) as client:
        api = SearchApi(client)
        resp = api.search(SearchRequest(query=query, limit=5))

    results = resp.results or []
    print(f"Found {len(results)} match(es) for {query}:")
    for r in results:
        print(f"  {r.qualified_name}  ({r.kind} in {r.file_path}:{r.start_line})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
