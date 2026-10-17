import { useCallback, useEffect, useRef, useState } from "react";
import { messages } from "@/i18n";
import { copyText } from "@/lib/clipboard";

const feedbackDurationMs = 2500;

/**
 * Copies text and shows a short-lived status message. The message belongs to
 * the value that was copied: once `value` changes, the stale message is hidden
 * without an effect resetting state.
 */
export function useCopyFeedback(value: string, successMessage: string) {
    const [feedback, setFeedback] = useState<{
        value: string;
        message: string;
    } | null>(null);
    const resetTimer = useRef<number | undefined>(undefined);

    useEffect(() => () => window.clearTimeout(resetTimer.current), []);

    const clear = useCallback(() => {
        window.clearTimeout(resetTimer.current);
        resetTimer.current = undefined;
        setFeedback(null);
    }, []);

    const copy = useCallback(async () => {
        if (!value) return;
        clear();
        const copied = await copyText(value);
        setFeedback({
            value,
            message: copied ? successMessage : messages().errors.copyUnavailable,
        });
        resetTimer.current = window.setTimeout(
            () => setFeedback(null),
            feedbackDurationMs,
        );
    }, [clear, successMessage, value]);

    const message = feedback?.value === value ? feedback.message : "";
    return { message, copy, clear };
}

/**
 * Copies one of several values on demand and shows a short-lived message for
 * the latest copy.
 */
export function useCopyStatus() {
    const [message, setMessage] = useState("");
    const resetTimer = useRef<number | undefined>(undefined);

    useEffect(() => () => window.clearTimeout(resetTimer.current), []);

    const copy = useCallback(async (value: string, successMessage: string) => {
        window.clearTimeout(resetTimer.current);
        setMessage("");
        const copied = await copyText(value);
        setMessage(copied ? successMessage : messages().errors.copyUnavailable);
        resetTimer.current = window.setTimeout(() => setMessage(""), feedbackDurationMs);
    }, []);

    return { message, copy };
}
