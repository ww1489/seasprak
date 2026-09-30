import { describe, expect, it } from "vitest";
import { buildInput, formFromSchema, type Field } from "./workflowForm";

describe("workflow form", () => {
  it("derives flat fields with required and enum", () => {
    const spec = formFromSchema({
      type: "object",
      properties: { topic: { type: "string" }, count: { type: "integer" }, ratio: { type: "number" }, fast: { type: "boolean" }, mode: { type: "string", enum: ["a", "b"] } },
      required: ["topic", "count"],
    });
    expect("fields" in spec).toBe(true);
    const fields = (spec as { fields: Field[] }).fields;
    expect(fields.map((f) => [f.name, f.kind, f.required])).toEqual([
      ["count", "integer", true],
      ["fast", "boolean", false],
      ["mode", "string", false],
      ["ratio", "number", false],
      ["topic", "string", true],
    ]);
    expect(fields.find((f) => f.name === "mode")?.enum).toEqual(["a", "b"]);
  });

  it("rejects nested or untyped properties instead of guessing", () => {
    expect(formFromSchema({ type: "object", properties: { x: { type: "object" } } })).toEqual({ unsupported: true });
    expect(formFromSchema({ type: "object", properties: { x: {} } })).toEqual({ unsupported: true });
    expect(formFromSchema({ type: "array" })).toEqual({ unsupported: true });
    expect(formFromSchema(undefined)).toEqual({ fields: [] });
  });

  it("builds typed input and reports invalid fields", () => {
    const fields: Field[] = [
      { name: "topic", kind: "string", required: true },
      { name: "count", kind: "integer", required: true },
      { name: "fast", kind: "boolean", required: false },
      { name: "mode", kind: "string", required: false, enum: ["a", "b"] },
    ];
    expect(buildInput(fields, { topic: "中文😀", count: "3", fast: "true" })).toEqual({ input: { topic: "中文😀", count: 3, fast: true } });
    expect(buildInput(fields, { topic: "x" })).toEqual({ invalid: "count" });
    expect(buildInput(fields, { topic: "x", count: "1.5" })).toEqual({ invalid: "count" });
    expect(buildInput(fields, { topic: "x", count: "1", mode: "c" })).toEqual({ invalid: "mode" });
  });
});
