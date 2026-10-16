import { LoaderCircle } from "lucide-react";
import { AuthCardShell } from "@/components/AuthCardShell";
import { CardDescription, CardHeader } from "@/components/ui/card";

export function LoadingCard({
    title = "Checking access",
    description,
}: {
    title?: string;
    description?: string;
} = {}) {
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="flex items-center gap-2.5 font-sans text-lg font-semibold uppercase tracking-[0.05em]">
                    <LoaderCircle className="size-5 animate-spin text-primary" />
                    {title}
                </h1>
                {description && <CardDescription>{description}</CardDescription>}
            </CardHeader>
        </AuthCardShell>
    );
}
