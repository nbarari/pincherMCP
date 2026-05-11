// Real-shaped JS fixture for the JS AST extractor cross-corpus check.
// Exercises every form the AST extractor distinguishes from regex:
// - ESM imports + named exports
// - export class with method-shorthand bodies
// - export const arrow function
// - object-literal arrow-method (React/Vue handler form)
// - IIFE recovery (top-level return inside view.extend pattern)

import { format } from 'node:util';
import { readFile } from 'node:fs/promises';

export class Greeting {
    constructor(target) {
        this.target = target;
    }

    say() {
        return format('hello %s', this.target);
    }

    async loadAndSay(path) {
        const data = await readFile(path, 'utf8');
        return this.say() + ': ' + data.trim();
    }
}

export const handler = (event) => {
    return new Greeting(event.target).say();
};

export const handlers = {
    onClick: function (ev) { return ev.target; },
    onChange: (ev) => ev.value,
    onMount() { /* shorthand */ },
};

// Module-private helper, NOT exported — should appear with IsExported=false
// under AST and the v0.20 ES2015+ semantics fix (#557).
const internalCounter = 0;
