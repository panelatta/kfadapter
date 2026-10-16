import { LoaderCircle } from "lucide-react";
import { AuthCardShell } from "@/components/AuthCardShell";
import { CardDescription, CardHeader } from "@/components/ui/card";
import { useI18n } from "@/i18n";

export function LoadingCard({
    title,
    description,
}: {
    title?: string;
    description?: string;
} = {}) {
    const { m } = useI18n();
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="flex items-center gap-2.5 text-lg font-semibold">
                    <LoaderCircle className="size-5 animate-spin text-primary" />
                    {title ?? m.access.checkingTitle}
                </h1>
                {description && <CardDescription>{description}</CardDescription>}
            </CardHeader>
        </AuthCardShell>
    );
}
