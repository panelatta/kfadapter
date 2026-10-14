import { RefreshCw } from "lucide-react";
import { AuthCardShell } from "@/components/AuthCardShell";
import { FormError } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { CardContent, CardDescription, CardHeader } from "@/components/ui/card";

/** Shown when the service status cannot be loaded for an unlocked console. */
export function ServiceErrorCard({
    message,
    onRetry,
}: {
    message: string;
    onRetry: () => void;
}) {
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="font-sans text-xl font-semibold uppercase tracking-[0.05em]">
                    Service unavailable
                </h1>
                <CardDescription>
                    The console could not load the service status.
                </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
                <FormError message={message} />
                <Button className="w-full" onClick={onRetry} type="button">
                    <RefreshCw className="size-4" />
                    Try again
                </Button>
            </CardContent>
        </AuthCardShell>
    );
}
