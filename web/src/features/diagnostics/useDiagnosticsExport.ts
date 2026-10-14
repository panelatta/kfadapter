import { useState } from "react";
import type { ApiClient } from "@/api";
import { describeError, isSessionProblem } from "@/lib/errors";

const reportFileName = "chinadapter-diagnostics.json";

/** Downloads the redacted diagnostics report produced by the service. */
export function useDiagnosticsExport({
    api,
    getSessionRevision,
    onAccessLost,
}: {
    api: ApiClient;
    getSessionRevision: () => number;
    onAccessLost: () => void;
}) {
    const [exporting, setExporting] = useState(false);
    const [error, setError] = useState("");

    const exportDiagnostics = async () => {
        const revision = getSessionRevision();
        setExporting(true);
        setError("");
        try {
            const report = await api.diagnostics();
            // A report requested before the console was locked is discarded.
            if (revision !== getSessionRevision()) return;
            const url = URL.createObjectURL(report);
            const link = document.createElement("a");
            link.href = url;
            link.download = reportFileName;
            link.click();
            URL.revokeObjectURL(url);
        } catch (cause) {
            if (revision !== getSessionRevision()) return;
            if (isSessionProblem(cause)) onAccessLost();
            else setError(describeError(cause));
        } finally {
            if (revision === getSessionRevision()) setExporting(false);
        }
    };

    return { error, exportDiagnostics, exporting };
}
