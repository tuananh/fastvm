import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  ApiError,
  createSandbox,
  deleteSandbox,
  getSandbox,
  getSandboxLog,
  getSandboxStats,
  getVersion,
  listEvents,
  listImages,
  listSandboxes,
  sleepSandbox,
  wakeSandbox,
} from "./api";

function fakeResponse(body: unknown, status = 200, statusText = "OK"): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText,
    text: async () => (body === undefined ? "" : JSON.stringify(body)),
  } as unknown as Response;
}

const fetchMock = vi.fn<typeof fetch>();

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function lastCall(): [string, RequestInit] {
  const call = fetchMock.mock.calls.at(-1);
  if (call === undefined) {
    throw new Error("fetch was not called");
  }
  const [input, init] = call;
  if (typeof input !== "string") {
    throw new Error("expected fetch to be called with a string path");
  }
  return [input, init ?? {}];
}

function bodyJSON(init: RequestInit): unknown {
  if (typeof init.body !== "string") {
    throw new Error("expected a JSON string request body");
  }
  return JSON.parse(init.body) as unknown;
}

describe("api requests", () => {
  it("fetches and parses the version with same-origin JSON headers", async () => {
    fetchMock.mockResolvedValue(fakeResponse({ version: "0.1.0", name: "fastvm" }));
    const version = await getVersion();

    expect(version.version).toBe("0.1.0");
    const [path, init] = lastCall();
    expect(path).toBe("/v1/version");
    expect(init).toMatchObject({
      headers: { Accept: "application/json" },
    });
  });

  it("lists sandboxes and tolerates extra fields", async () => {
    fetchMock.mockResolvedValue(
      fakeResponse([
        {
          id: "sb-1",
          name: "web",
          state: "Running",
          base_disk: "sha256:abc",
          vcpus: 1,
          memory_mb: 256,
          wake_count: 0,
          no_auto_sleep: false,
          last_activity_at: "2026-11-17T08:00:00Z",
          created_at: "2026-11-17T08:00:00Z",
          updated_at: "2026-11-17T08:00:00Z",
          unlisted: "field",
        },
      ]),
    );

    const sandboxes = await listSandboxes();
    expect(sandboxes).toHaveLength(1);
    expect(sandboxes[0].state).toBe("Running");
    expect(lastCall()[0]).toBe("/v1/sandboxes");
  });

  it("URL-encodes sandbox refs and fetches stats", async () => {
    fetchMock.mockResolvedValue(
      fakeResponse({
        sandbox_id: "sb 1",
        name: "web",
        state: "Idle",
        cpu_percent: 0,
        vcpus: 1,
        memory_mb: 256,
        memory_rss_bytes: 1024,
        uptime_seconds: 10,
        wake_count: 2,
        block_read_bytes: 0,
        block_write_bytes: 0,
        rx_bytes: 0,
        tx_bytes: 0,
        captured_at: "2026-11-17T08:00:00Z",
      }),
    );

    const stats = await getSandboxStats("sb 1");
    expect(stats.sandbox_id).toBe("sb 1");
    expect(lastCall()[0]).toBe("/v1/sandboxes/sb%201/stats");
  });

  it("lists images and limits events", async () => {
    fetchMock.mockResolvedValue(fakeResponse([]));
    await listImages();
    expect(lastCall()[0]).toBe("/v1/images");

    fetchMock.mockResolvedValue(fakeResponse([]));
    await listEvents(5);
    expect(lastCall()[0]).toBe("/v1/events?limit=5");
  });

  it("sends JSON bodies for create and sleep", async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(
        {
          id: "sb-2",
          name: "job",
          state: "Creating",
          base_disk: "sha256:abc",
          vcpus: 1,
          memory_mb: 256,
          wake_count: 0,
          no_auto_sleep: true,
          last_activity_at: "2026-11-17T08:00:00Z",
          created_at: "2026-11-17T08:00:00Z",
          updated_at: "2026-11-17T08:00:00Z",
        },
        201,
        "Created",
      ),
    );
    const created = await createSandbox({
      name: "job",
      image: "demo:latest",
      vcpus: 1,
      memory_mb: 256,
      cmd: ["/bin/sleep", "3600"],
      no_auto_sleep: true,
    });
    expect(created.id).toBe("sb-2");
    const [path, init] = lastCall();
    expect(path).toBe("/v1/sandboxes");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ "Content-Type": "application/json" });
    expect(bodyJSON(init)).toEqual({
      name: "job",
      image: "demo:latest",
      vcpus: 1,
      memory_mb: 256,
      cmd: ["/bin/sleep", "3600"],
      no_auto_sleep: true,
    });

    fetchMock.mockResolvedValue(fakeResponse(undefined, 204, "No Content"));
    await expect(sleepSandbox("web", true)).resolves.toBeUndefined();
    const [sleepPath, sleepInit] = lastCall();
    expect(sleepPath).toBe("/v1/sandboxes/web/sleep");
    expect(bodyJSON(sleepInit)).toEqual({ deep: true });
  });

  it("sends wake/delete requests with no body and handles 204", async () => {
    fetchMock.mockResolvedValue(fakeResponse(undefined, 204, "No Content"));
    await expect(wakeSandbox("web")).resolves.toBeUndefined();
    expect(lastCall()[1].method).toBe("POST");
    expect(lastCall()[1].body).toBeUndefined();

    fetchMock.mockResolvedValue(fakeResponse(undefined, 204, "No Content"));
    await expect(deleteSandbox("web")).resolves.toBeUndefined();
    expect(lastCall()[0]).toBe("/v1/sandboxes/web");
    expect(lastCall()[1].method).toBe("DELETE");
  });
});

describe("ApiError mapping", () => {
  it("maps a JSON error envelope to ApiError status/message/kind", async () => {
    fetchMock.mockResolvedValue(
      fakeResponse({ error: "sandbox not found: nope", kind: "not_found" }, 404, "Not Found"),
    );

    const error = await getSandbox("nope").catch((err: unknown) => err);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({
      name: "ApiError",
      status: 404,
      message: "sandbox not found: nope",
      kind: "not_found",
    });
  });

  it("treats a missing guest log file as empty", async () => {
    fetchMock.mockResolvedValue(
      fakeResponse(
        { error: "stat guest file: no such file or directory" },
        500,
        "Internal Server Error",
      ),
    );

    await expect(getSandboxLog("web", "app")).resolves.toBe("");
  });

  it("falls back to the HTTP status for non-JSON error bodies", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 502,
      statusText: "Bad Gateway",
      text: async () => "upstream exploded",
    } as unknown as Response);

    const error = await listSandboxes().catch((err: unknown) => err);
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({ status: 502, message: "HTTP 502 Bad Gateway" });
  });

  it("rejects a malformed success body", async () => {
    fetchMock.mockResolvedValue(fakeResponse({ unexpected: true }));
    await expect(getVersion()).rejects.toThrow();
  });
});
