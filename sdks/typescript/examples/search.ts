// Minimal example: call `pincher search` from TypeScript via the
// generated SDK. Assumes you've run `scripts/generate-sdks.sh
// typescript` against a running pincher.
//
// Run with:
//   PINCHER_HTTP_URL=http://localhost:8080 \
//   PINCHER_HTTP_KEY=$(cat ~/.pincher-key) \
//   npx tsx sdks/typescript/examples/search.ts "ProcessPayment"

import { Configuration, SearchApi } from "../generated";

async function main() {
  const baseURL = process.env.PINCHER_HTTP_URL ?? "http://localhost:8080";
  const apiKey = process.env.PINCHER_HTTP_KEY;
  const query = process.argv[2] ?? "Hello";

  const config = new Configuration({
    basePath: baseURL,
    accessToken: apiKey,
  });
  const api = new SearchApi(config);

  const resp = await api.search({
    searchRequest: { query, limit: 5 },
  });

  console.log(`Found ${resp.results?.length ?? 0} match(es) for ${query}:`);
  for (const r of resp.results ?? []) {
    console.log(`  ${r.qualified_name}  (${r.kind} in ${r.file_path}:${r.start_line})`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
