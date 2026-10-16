import type { StatusResponse } from "@/types";
import { AuthCardShell } from "@/components/AuthCardShell";
import { CardContent, CardHeader } from "@/components/ui/card";
import { useI18n } from "@/i18n";
import { ProviderCredentialsForm } from "./ProviderCredentialsForm";

export function ProviderLoginCard({
    onSubmit,
    error,
    providers,
    providerAuth,
}: {
    onSubmit: (
        provider: string,
        fields: Record<string, string>,
    ) => Promise<void>;
    error: string;
    providers: string[];
    providerAuth: StatusResponse["providerAuth"];
}) {
    const { m } = useI18n();
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="text-xl font-semibold tracking-[-0.01em]">
                    {m.providers.loginTitle}
                </h1>
            </CardHeader>
            <CardContent>
                <ProviderCredentialsForm
                    error={error}
                    idPrefix="initial"
                    onSubmit={onSubmit}
                    providers={providers}
                    providerAuth={providerAuth}
                />
            </CardContent>
        </AuthCardShell>
    );
}
