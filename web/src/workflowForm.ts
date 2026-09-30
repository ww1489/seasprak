// Builds a flat form from a workflow inputSchema. Only top-level properties of
// type string/number/integer/boolean (optionally with enum) are supported;
// anything else makes the schema unsupported so the page can say so instead
// of guessing. Values are validated here and again by the server.

export type FieldKind = "string" | "number" | "integer" | "boolean";
export type Field = { name: string; kind: FieldKind; required: boolean; enum?: (string | number | boolean)[]; description?: string };
export type FormSpec = { fields: Field[] } | { unsupported: true };

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

export function formFromSchema(schema: unknown): FormSpec {
  if (schema === undefined || schema === null) return { fields: [] };
  if (!isObj(schema) || (schema.type !== undefined && schema.type !== "object")) return { unsupported: true };
  const props = schema.properties === undefined ? {} : schema.properties;
  if (!isObj(props)) return { unsupported: true };
  const required = Array.isArray(schema.required) ? schema.required.filter((x): x is string => typeof x === "string") : [];
  const fields: Field[] = [];
  for (const name of Object.keys(props).sort()) {
    const p = props[name];
    if (!isObj(p)) return { unsupported: true };
    const kind = p.type;
    if (kind !== "string" && kind !== "number" && kind !== "integer" && kind !== "boolean") return { unsupported: true };
    const f: Field = { name, kind, required: required.includes(name) };
    if (typeof p.description === "string") f.description = p.description;
    if (p.enum !== undefined) {
      if (!Array.isArray(p.enum) || p.enum.length === 0 || !p.enum.every((x) => ["string", "number", "boolean"].includes(typeof x))) return { unsupported: true };
      f.enum = p.enum as (string | number | boolean)[];
    }
    fields.push(f);
  }
  return { fields };
}

/**
 * buildInput converts raw form strings to typed values. It returns the input
 * object or the name of the first invalid field. Empty optional fields are
 * omitted; booleans use "true"/"false".
 */
export function buildInput(fields: Field[], values: Record<string, string>): { input: Record<string, unknown> } | { invalid: string } {
  const input: Record<string, unknown> = {};
  for (const f of fields) {
    const raw = values[f.name] ?? "";
    if (raw === "") {
      if (f.required) return { invalid: f.name };
      continue;
    }
    let v: unknown;
    switch (f.kind) {
      case "string":
        v = raw;
        break;
      case "boolean":
        if (raw !== "true" && raw !== "false") return { invalid: f.name };
        v = raw === "true";
        break;
      default: {
        const n = Number(raw);
        if (raw.trim() === "" || !Number.isFinite(n) || (f.kind === "integer" && !Number.isSafeInteger(n))) return { invalid: f.name };
        v = n;
      }
    }
    if (f.enum && !f.enum.some((e) => e === v)) return { invalid: f.name };
    input[f.name] = v;
  }
  return { input };
}
