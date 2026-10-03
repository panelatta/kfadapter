import { useState } from "react";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { NodeList } from "./components/NodeList";
import { TooltipProvider } from "./components/ui/tooltip";
import { makeApi, nodes } from "./test/fixtures";
import type { NodeRecord } from "./types";

function renderProbeFailure(error: unknown) {
  const api = makeApi();
  vi.mocked(api.probeNode).mockRejectedValueOnce(error);
  let observed: NodeRecord[] = [];
  function Harness() {
    const [inventory, setInventory] = useState([nodes[0]]);
    observed = inventory;
    return <TooltipProvider><NodeList api={api} getSessionRevision={() => 1} nodes={inventory} onAccessLost={vi.fn()} onNodesChange={setInventory} /></TooltipProvider>;
  }
  render(<Harness />);
  return { api, latestNode: () => observed[0] };
}

async function probeFirstNode(all: boolean) {
  if (all) {
    await userEvent.click(screen.getByRole("button", { name: "Check TCP reachability for all nodes" }));
  } else {
    const group = screen.getByRole("group", { name: "East China group" });
    await userEvent.click(within(group).getByText("East China", { exact: true }));
    await userEvent.click(screen.getByRole("button", { name: "Check TCP reachability for Shanghai 01" }));
  }
}

describe("probe failure classifications", () => {
  for (const all of [false, true]) {
    describe(all ? "probe all" : "single probe", () => {
      it.each([
        ["stale_probe", 409, "Changed"],
        ["probe_busy", 429, "Busy"],
        ["node_ineligible", 409, "Ineligible"],
        ["node_not_found", 404, "Unavailable"],
        ["node_snapshot_unavailable", 409, "Unavailable"],
        ["internal_error", 500, "Failed"],
      ])("preserves health and latency for %s", async (code, status, label) => {
        const { latestNode } = renderProbeFailure(new ApiError({ title: "Probe was not applied", status, code }));
        await probeFirstNode(all);
        await waitFor(() => expect(latestNode().probeError).toBe(label));
        expect(latestNode().health).toBe("healthy");
        expect(latestNode().tcpLatencyMs).toBe(76);
      });

      it.each(["tcp_probe_failed", "tcp_probe_timeout"])("records unhealthy only for an upstream %s", async (code) => {
        const { latestNode } = renderProbeFailure(new ApiError({ title: "TCP connection failed", status: 502, code }));
        await probeFirstNode(all);
        await waitFor(() => expect(latestNode().health).toBe("unhealthy"));
        expect(latestNode().tcpLatencyMs).toBeUndefined();
      });

      it("preserves upstream health when the console request has a network error", async () => {
        const { latestNode } = renderProbeFailure(new TypeError("Failed to fetch"));
        await probeFirstNode(all);
        await waitFor(() => expect(latestNode().probeError).toBe("Failed"));
        expect(latestNode().health).toBe("healthy");
        expect(latestNode().tcpLatencyMs).toBe(76);
      });
    });
  }
});
