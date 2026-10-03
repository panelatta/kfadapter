export async function copyText(value: string): Promise<boolean> {
    try {
        if (navigator.clipboard?.writeText) {
            await navigator.clipboard.writeText(value);
            return true;
        }
    } catch {
        // Fall through to the legacy copy command for non-secure LAN origins.
    }

    // Keep the temporary field next to the focused control so selecting it does
    // not move focus out of an open popover (which would dismiss it), and put
    // focus back afterwards.
    const previousFocus =
        document.activeElement instanceof HTMLElement
            ? document.activeElement
            : null;
    const container = previousFocus?.parentElement ?? document.body;
    const textArea = document.createElement("textarea");
    textArea.value = value;
    textArea.setAttribute("readonly", "");
    textArea.setAttribute("aria-hidden", "true");
    textArea.tabIndex = -1;
    textArea.style.position = "fixed";
    textArea.style.opacity = "0";
    textArea.style.pointerEvents = "none";
    container.append(textArea);
    textArea.select();
    try {
        return document.execCommand("copy");
    } catch {
        return false;
    } finally {
        textArea.remove();
        previousFocus?.focus({ preventScroll: true });
    }
}
