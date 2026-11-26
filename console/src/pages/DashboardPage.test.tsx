import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { AuthContext } from "../auth/context";
import type { AuthContextValue } from "../auth/context";
import { installDomStubs } from "../components/test-dom";
import { ThemeProvider } from "../theme/ThemeProvider";
import { ToastProvider } from "../ui";
import DashboardPage from "./DashboardPage";

function fakeResponse(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    statusText: "OK",
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

const SANDBOX = {
  id: "sandbox-1",
  name: "web-1",
  state: "Running",
  base_disk: "sha256:abcdef",
  vcpus: 2,
  memory_mb: 512,
  ip: "10.0.0.7",
  wake_count: 3,
  no_auto_sleep: false,
  last_activity_at: "2024-06-15T12:34:56Z",
  created_at: "2024-06-15T12:30:00Z",
  updated_at: "2024-06-15T12:34:56Z",
  ports: [{ host_port: 8080, guest_port: 80, proto: "tcp" }],
};

const STATS = {
  sandbox_id: SANDBOX.id,
  name: SANDBOX.name,
  state: "Running",
  cpu_percent: 42.5,
  vcpus: 2,
  memory_mb: 512,
  memory_rss_bytes: 128 * 1024 * 1024,
  uptime_seconds: 300,
  wake_count: 3,
  block_read_bytes: 1024,
  block_write_bytes: 2048,
  rx_bytes: 4096,
  tx_bytes: 8192,
  captured_at: "2024-06-15T12:34:56Z",
};

const IMAGES = [
  {
    name: "demo",
    tags: ["demo:latest"],
    digest: "sha256:abcdef",
    size_bytes: 64 * 1024 * 1024,
    source: "docker",
    created_at: "2024-06-01T00:00:00Z",
  },
];

const EVENTS = [
  {
    time: "2024-06-15T12:34:56Z",
    sandbox_id: SANDBOX.id,
    sandbox_name: SANDBOX.name,
    type: "woken",
    message: "sandbox woken by inbound traffic",
  },
];

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  installDomStubs();
  fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.includes("/v1/version")) {
      return fakeResponse({ version: "0.1.0", name: "fastvm" });
    }
    if (url.includes("/v1/images")) {
      return fakeResponse(IMAGES);
    }
    if (url.endsWith("/v1/sandboxes/stats")) {
      return fakeResponse([STATS]);
    }
    if (url.includes("/stats")) {
      return fakeResponse(STATS);
    }
    if (url.includes("/v1/events")) {
      return fakeResponse(EVENTS);
    }
    if (url.includes("/v1/sandboxes")) {
      return fakeResponse([SANDBOX]);
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const auth: AuthContextValue = {
  status: "authenticated",
  user: { subject: "sub", name: "Demo User", roles: ["admin"] },
  accessToken: "token",
  error: null,
  login: vi.fn(),
  logout: vi.fn(),
};

function renderDashboard() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  return render(
    <ThemeProvider>
      <ToastProvider>
        <QueryClientProvider client={queryClient}>
          <AuthContext.Provider value={auth}>
            <DashboardPage />
          </AuthContext.Provider>
        </QueryClientProvider>
      </ToastProvider>
    </ThemeProvider>,
  );
}

describe("DashboardPage integration", () => {
  it("renders the version, summary, table row and live stats", async () => {
    renderDashboard();

    expect(await screen.findByText("web-1")).toBeInTheDocument();
    expect(await screen.findByText("fastvm v0.1.0")).toBeInTheDocument();
    expect(await screen.findByText("42.5%")).toBeInTheDocument();
    expect(screen.getByText("Total VMs")).toBeInTheDocument();
    expect(screen.getByText("10.0.0.7")).toBeInTheDocument();
  });

  it("opens the create VM modal", async () => {
    renderDashboard();
    fireEvent.click(await screen.findByRole("button", { name: "Create VM" }));
    expect(await screen.findByText("Create virtual machine")).toBeInTheDocument();
    expect(await screen.findByText("Base image")).toBeInTheDocument();
  });

  it("navigates to the Images page", async () => {
    renderDashboard();

    fireEvent.click(await screen.findByRole("link", { name: "Images" }));

    expect(await screen.findByText("Base images")).toBeInTheDocument();
    expect(screen.queryByText("Total VMs")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Pull image" }));
    expect(
      await screen.findByText(
        "Fetch an OCI image and build a bootable base disk. The first pull can take a while.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Image reference")).toBeInTheDocument();
    expect(screen.getByLabelText("Size (GiB, optional)")).toBeInTheDocument();
  });

  it("creates a VM from a selected image with the image preselected", async () => {
    window.history.replaceState(null, "", "#");
    renderDashboard();

    fireEvent.click(await screen.findByRole("link", { name: "Images" }));
    fireEvent.click(await screen.findByRole("button", { name: "Create VM from demo:latest" }));

    expect(await screen.findByText("Create virtual machine")).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: /Base image/ })).toHaveValue("sha256:abcdef");
  });

  it("opens the pull dialog from the empty Create VM image hint", async () => {
    window.history.replaceState(null, "", "#");
    const original = fetchMock.getMockImplementation() as
      | ((input: RequestInfo | URL) => Promise<Response>)
      | undefined;
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/images")) {
        return fakeResponse([]);
      }
      if (original !== undefined) {
        return original(input);
      }
      throw new Error(`unexpected fetch ${url}`);
    });

    renderDashboard();
    fireEvent.click(await screen.findByRole("button", { name: "Create VM" }));
    expect(await screen.findByText(/No base disk images/)).toBeInTheDocument();
    const imageLinks = screen.getAllByRole("link", { name: "Images" });
    fireEvent.click(imageLinks[imageLinks.length - 1]);

    expect(await screen.findByLabelText("Image reference")).toBeInTheDocument();
    expect(screen.getByLabelText("Size (GiB, optional)")).toBeInTheDocument();
  });

  it("renders the sidebar shell and toggles the mobile sidebar", async () => {
    renderDashboard();

    const toggle = await screen.findByRole("button", { name: "Open sidebar" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls", "app-sidebar");

    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("navigation", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sandboxes" })).toBeInTheDocument();
  });
});
