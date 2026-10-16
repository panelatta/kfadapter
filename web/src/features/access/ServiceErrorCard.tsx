import { RefreshCw } from "lucide-react";
import { AuthCardShell } from "@/components/AuthCardShell";
import { FormError } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { useI18n } from "@/i18n";

/** Shown when the service status cannot be loaded for an unlocked console. */
export function ServiceErrorCard({
    message,
    onRetry,
}: {
    message: string;
    onRetry: () => void;
}) {
    const { m } = useI18n();
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="text-xl font-semibold tracking-[-0.01em]">
                    {m.access.unavailableTitle}
                </h1>
                <CardDescription>
                    {m.access.unavailableDescription}
                </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
                <FormError message={message} />
                <Button className="w-full" onClick={onRetry} type="button">
                    <RefreshCw className="size-4" />
                    {m.access.tryAgain}
                </Button>
            </CardContent>
        </AuthCardShell>
    );
}
