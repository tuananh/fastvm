import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { ToastProvider } from "../ui";
import { CreateVmModal } from "./CreateVmModal";
import { installDomStubs } from "./test-dom";

interface CreateSandboxMock {
  mutateAsync: ReturnType<typeof vi.fn>;
}

const queryMocks = vi.hoisted(
  (): { create: CreateSandboxMock; images: unknown[]; kernels: unknown[] } => ({
    create: { mutateAsync: vi.fn() },
    kernels: [
      {
        name: "ch-release-v7.2.8-20261009-vmlinux-x86_64",
        release: "ch-release-v7.2.8-20261009",
        asset: "vmlinux-x86_64",
        size_bytes: 48 * 1024 * 1024,
        active: true,
      },
    ],
    images: [
      {
        name: "demo",
        tags: ["demo:latest"],
        digest: "sha256:abc123",
        size_bytes: 64 * 1024 * 1024,
        source: "docker",
        created_at: "2024-06-01T00:00:00Z",
      },
    ],
  }),
);

vi.mock("../lib/queries", () => ({
  useImages: () => ({
    data: queryMocks.images,
    isPending: false,
    isError: false,
    error: null,
  }),
  useCreateSandbox: () => ({
    mutateAsync: queryMocks.create.mutateAsync,
    isPending: false,
  }),
  useKernels: () => ({
    data: { dir: "/tmp/fastvm/kernels", kernels: queryMocks.kernels },
    isPending: false,
    isError: false,
    error: null,
  }),
}));

beforeEach(() => {
  installDomStubs();
  queryMocks.create.mutateAsync.mockReset();
  queryMocks.create.mutateAsync.mockResolvedValue({ id: "sb-1", name: "web-1" });
});

afterEach(cleanup);

function renderModal() {
  const onClose = vi.fn();
  const utils = render(
    <ToastProvider>
      <CreateVmModal open onClose={onClose} />
    </ToastProvider>,
  );
  return { ...utils, onClose };
}

function chooseImage() {
  fireEvent.change(screen.getByRole("combobox", { name: /Base image/ }), {
    target: { value: "sha256:abc123" },
  });
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Create VM" }));
}

describe("CreateVmModal host volumes", () => {
  it("sends the collected host volumes with the create payload", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add volume" }));
    fireEvent.change(screen.getByPlaceholderText("/host/dir"), {
      target: { value: "/srv/data" },
    });
    fireEvent.change(screen.getByPlaceholderText("/mnt/data"), {
      target: { value: "/mnt/data" },
    });
    fireEvent.click(screen.getByRole("switch", { name: "Read-only" }));

    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as {
      volumes?: unknown[];
    };
    expect(input.volumes).toEqual([
      { host_path: "/srv/data", guest_path: "/mnt/data", read_only: true },
    ]);
  });

  it("omits volumes when the list is empty", async () => {
    renderModal();
    chooseImage();
    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as Record<string, unknown>;
    expect("volumes" in input).toBe(false);
  });

  it("supports multiple rows and removing one", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add volume" }));
    fireEvent.click(screen.getByRole("button", { name: "Add volume" }));
    expect(screen.getAllByPlaceholderText("/host/dir")).toHaveLength(2);

    const hostInputs = screen.getAllByPlaceholderText("/host/dir");
    const guestInputs = screen.getAllByPlaceholderText("/mnt/data");
    fireEvent.change(hostInputs[0], { target: { value: "/srv/one" } });
    fireEvent.change(guestInputs[0], { target: { value: "/mnt/one" } });

    fireEvent.click(screen.getAllByRole("button", { name: "Remove volume" })[1]);
    expect(screen.getAllByPlaceholderText("/host/dir")).toHaveLength(1);

    submit();
    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as {
      volumes?: unknown[];
    };
    expect(input.volumes).toEqual([
      { host_path: "/srv/one", guest_path: "/mnt/one", read_only: false },
    ]);
  });

  it("validates absolute host and guest paths before submitting", async () => {
    renderModal();
    chooseImage();
    fireEvent.click(screen.getByRole("button", { name: "Add volume" }));

    fireEvent.change(screen.getByPlaceholderText("/host/dir"), {
      target: { value: "relative/path" },
    });
    fireEvent.change(screen.getByPlaceholderText("/mnt/data"), {
      target: { value: "/mnt/with space" },
    });
    submit();

    expect(
      await screen.findByText("Host path must be absolute (start with /)."),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Absolute path, no spaces, ':' or ','.").length).toBeGreaterThan(0);
    expect(queryMocks.create.mutateAsync).not.toHaveBeenCalled();
  });

  it("rejects duplicate data volume mount paths", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    const mounts = screen.getAllByLabelText("Mount path");
    fireEvent.change(mounts[0], { target: { value: "/data" } });
    fireEvent.change(mounts[1], { target: { value: "/data" } });

    submit();

    expect(await screen.findByText("Data volume mount paths must be unique.")).toBeInTheDocument();
    expect(queryMocks.create.mutateAsync).not.toHaveBeenCalled();
  });
});

describe("CreateVmModal data volumes", () => {
  it("sends block data volumes with the create payload", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    fireEvent.change(screen.getByLabelText("Size (GiB)"), {
      target: { value: "16" },
    });
    fireEvent.change(screen.getByLabelText("Mount path"), {
      target: { value: "/workspace" },
    });

    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as {
      data_volumes?: unknown[];
    };
    expect(input.data_volumes).toEqual([{ size_gb: 16, mount: "/workspace" }]);
  });

  it("validates size, mount path, and duplicate mounts", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    const sizes = screen.getAllByLabelText("Size (GiB)");
    const mounts = screen.getAllByLabelText("Mount path");
    fireEvent.change(sizes[0], { target: { value: "0" } });
    fireEvent.change(mounts[0], { target: { value: "relative" } });
    fireEvent.change(mounts[1], { target: { value: "relative" } });

    submit();

    expect(
      await screen.findByText("Size must be a whole number between 1 and 1024 GiB."),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Absolute path, no spaces, ':' or ','.").length).toBeGreaterThan(0);
    expect(queryMocks.create.mutateAsync).not.toHaveBeenCalled();
  });

  it("rejects duplicate data volume mount paths", async () => {
    renderModal();
    chooseImage();

    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    fireEvent.click(screen.getByRole("button", { name: "Add data volume" }));
    const mounts = screen.getAllByLabelText("Mount path");
    fireEvent.change(mounts[0], { target: { value: "/data" } });
    fireEvent.change(mounts[1], { target: { value: "/data" } });

    submit();

    expect(await screen.findByText("Data volume mount paths must be unique.")).toBeInTheDocument();
    expect(queryMocks.create.mutateAsync).not.toHaveBeenCalled();
  });
});

describe("CreateVmModal empty image store", () => {
  it("links to the Images page when no base images exist", async () => {
    const previousImages = queryMocks.images;
    queryMocks.images = [];
    const onGoToImages = vi.fn();
    try {
      render(
        <ToastProvider>
          <CreateVmModal open onClose={vi.fn()} onGoToImages={onGoToImages} />
        </ToastProvider>,
      );

      expect(await screen.findByText(/No base disk images/)).toBeInTheDocument();
      fireEvent.click(screen.getByRole("link", { name: "Images" }));
      expect(onGoToImages).toHaveBeenCalledTimes(1);
    } finally {
      queryMocks.images = previousImages;
    }
  });
});

describe("CreateVmModal kernel", () => {
  it("sends the selected kernel release", async () => {
    renderModal();
    chooseImage();
    fireEvent.change(screen.getByRole("combobox", { name: /Kernel/ }), {
      target: { value: "ch-release-v7.2.8-20261009" },
    });
    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as { kernel?: string };
    expect(input.kernel).toBe("ch-release-v7.2.8-20261009");
  });

  it("omits the kernel when the daemon default is selected", async () => {
    renderModal();
    chooseImage();
    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as { kernel?: string };
    expect(input.kernel).toBeUndefined();
  });
});

describe("CreateVmModal max resources", () => {
  it("sends hotplug ceilings when provided", async () => {
    renderModal();
    chooseImage();
    fireEvent.change(screen.getByLabelText(/Max CPU/), { target: { value: "4" } });
    fireEvent.change(screen.getByLabelText(/Max memory/), { target: { value: "1024" } });
    submit();

    await waitFor(() => {
      expect(queryMocks.create.mutateAsync).toHaveBeenCalledTimes(1);
    });
    const input = queryMocks.create.mutateAsync.mock.calls[0][0] as {
      max_vcpus?: number;
      max_memory_mb?: number;
    };
    expect(input.max_vcpus).toBe(4);
    expect(input.max_memory_mb).toBe(1024);
  });
});
