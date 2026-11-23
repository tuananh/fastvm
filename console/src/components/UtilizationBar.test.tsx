import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vite-plus/test";
import { UtilizationBar } from "./UtilizationBar";
import { installDomStubs } from "./test-dom";

afterEach(cleanup);

describe("UtilizationBar", () => {
  it("renders the label, clamped value and detail", () => {
    installDomStubs();
    render(<UtilizationBar label="CPU" percent={128.4} detail="2 vCPUs" />);
    expect(screen.getByText("CPU")).toBeInTheDocument();
    expect(screen.getByText("100.0%")).toBeInTheDocument();
    expect(screen.getByText("2 vCPUs")).toBeInTheDocument();
  });

  it("clamps negative values to zero", () => {
    installDomStubs();
    render(<UtilizationBar label="Memory" percent={-12} />);
    expect(screen.getByText("Memory")).toBeInTheDocument();
    expect(screen.getByText("0.0%")).toBeInTheDocument();
  });

  it("keeps in-range values unmodified and marks the data attribute", () => {
    installDomStubs();
    render(<UtilizationBar label="CPU" percent={42.5} />);
    expect(screen.getByText("42.5%")).toBeInTheDocument();
    const bar = screen.getByRole("progressbar", { name: "CPU" });
    expect(bar).toHaveAttribute("aria-valuenow", "42.5");
    expect(bar.closest("[data-percent]")).toHaveAttribute("data-percent", "42.5");
  });
});

describe("UtilizationBar history graph", () => {
  it("renders one bar per distinct sample", () => {
    installDomStubs();
    const { rerender } = render(<UtilizationBar label="CPU" percent={10} showHistory />);
    rerender(<UtilizationBar label="CPU" percent={20} showHistory />);
    rerender(<UtilizationBar label="CPU" percent={30} showHistory />);
    const graph = screen.getByTestId("usage-history");
    expect(graph).toHaveAttribute("aria-valuenow", "30");
    expect(graph.children).toHaveLength(3);
  });
});
