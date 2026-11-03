# ECP schema

[`schema.ts`](schema.ts) is the normative definition of the Engram Context Protocol wire format. The prose specification is [docs/spec/ecp-v0.md](../docs/spec/ecp-v0.md); where they disagree, the schema wins.

## Generating JSON Schema

```bash
npx ts-json-schema-generator --path spec/schema.ts --type '*' --no-type-check --out spec/schema.json
```

Implementations in any language validate messages against the generated `schema.json`. The conformance suite (planned) will use it too.
