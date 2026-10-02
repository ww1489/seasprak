/// <reference types="node" />
// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";

const fileRead = vi.hoisted(() => vi.fn(() => { throw new Error("browser test attempted forbidden file access"); }));
vi.mock("node:fs", () => ({ readFileSync: fileRead }));

import { loadOpenAI } from "../../tests/live-openai-proxy";

afterEach(() => {
  vi.unstubAllEnvs();
  fileRead.mockClear();
});

function modelEnvironment() {
  vi.stubEnv("OPENAI_MODEL", "synthetic-test-model");
  vi.stubEnv("OPENAI_API_KEY", "synthetic-test-value");
  vi.stubEnv("OPENAI_BASE_URL", "https://example.invalid/model-api");
}

describe("browser acceptance credential source", () => {
  it("uses only the supplied process environment without reading a local file", () => {
    modelEnvironment();
    expect(loadOpenAI()).toEqual({ model: "synthetic-test-model", key: "synthetic-test-value", endpoint: "https://example.invalid/model-api/v1" });
    expect(fileRead).not.toHaveBeenCalled();
  });

  it("fails closed for missing configuration without probing files", () => {
    modelEnvironment();
    vi.stubEnv("OPENAI_API_KEY", "");
    expect(() => loadOpenAI()).toThrow("浏览器验收需要宿主环境中的 OPENAI_MODEL、OPENAI_API_KEY 和 OPENAI_BASE_URL");
    expect(fileRead).not.toHaveBeenCalled();
  });

  it.each(["file:///private/config", "https://user:synthetic@example.invalid/v1", "https://example.invalid/v1?key=synthetic", "https://example.invalid/v1#fragment"])("rejects unsafe endpoint %s before any request", (endpoint) => {
    modelEnvironment();
    vi.stubEnv("OPENAI_BASE_URL", endpoint);
    expect(() => loadOpenAI()).toThrow("浏览器验收环境中的 OPENAI_BASE_URL 不安全");
    expect(fileRead).not.toHaveBeenCalled();
  });
});
