import { useState } from "react";
import type { FormEvent } from "react";
import { LoaderCircle } from "lucide-react";
import { AuthCardShell } from "@/components/AuthCardShell";
import { Field, FormError } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/i18n";
import { describeError } from "@/lib/errors";

export function AccessTokenCard({
    mode,
    onSubmit,
    error,
    setupAllowed = true,
}: {
    mode: "setup" | "login";
    onSubmit: (token: string) => Promise<void>;
    error: string;
    /** False when this browser is not on the adapter host (setup is loopback-only). */
    setupAllowed?: boolean;
}) {
    const { m } = useI18n();
    const [token, setToken] = useState("");
    const [confirmation, setConfirmation] = useState("");
    const [submitting, setSubmitting] = useState(false);
    const [localError, setLocalError] = useState("");
    const isSetup = mode === "setup";

    const clearFields = () => {
        setToken("");
        setConfirmation("");
    };

    const submit = async (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const submittedToken = token.trim();
        const submittedConfirmation = confirmation.trim();
        clearFields();

        const tokenBytes = new TextEncoder().encode(submittedToken).byteLength;
        if (tokenBytes < 16 || tokenBytes > 128) {
            setLocalError(m.access.tokenLengthInvalid);
            return;
        }
        if (isSetup && submittedToken !== submittedConfirmation) {
            setLocalError(m.access.tokensMismatch);
            return;
        }

        setSubmitting(true);
        setLocalError("");
        try {
            await onSubmit(submittedToken);
        } catch (requestError) {
            setLocalError(describeError(requestError));
        } finally {
            clearFields();
            setSubmitting(false);
        }
    };

    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="text-xl font-semibold tracking-[-0.01em]">
                    {isSetup ? m.access.setupTitle : m.access.loginTitle}
                </h1>
                {isSetup ? (
                    <CardDescription>
                        {m.access.setupDescription}
                    </CardDescription>
                ) : null}
            </CardHeader>
            <CardContent>
                {isSetup && !setupAllowed ? (
                    <p
                        className="rounded-md border border-led-warn/30 bg-warning px-3.5 py-3 text-sm leading-6 text-warning-foreground"
                        role="alert"
                    >
                        {m.access.setupLoopbackOnly}
                    </p>
                ) : (
                    <form className="grid gap-4" noValidate onSubmit={submit}>
                        <FormError message={localError || error} />
                        <Field label={m.access.tokenLabel} name="access-token">
                            <Input
                                autoComplete="off"
                                id="access-token"
                                name="access-token"
                                onChange={(event) =>
                                    setToken(event.currentTarget.value)
                                }
                                type="password"
                                value={token}
                            />
                        </Field>
                        {isSetup ? (
                            <Field
                                label={m.access.confirmTokenLabel}
                                name="access-token-confirmation"
                            >
                                <Input
                                    autoComplete="off"
                                    id="access-token-confirmation"
                                    name="access-token-confirmation"
                                    onChange={(event) =>
                                        setConfirmation(event.currentTarget.value)
                                    }
                                    type="password"
                                    value={confirmation}
                                />
                            </Field>
                        ) : null}
                        <Button
                            className="mt-2 w-full"
                            disabled={submitting}
                            type="submit"
                        >
                            {submitting ? (
                                <>
                                    <LoaderCircle className="size-4 animate-spin" />
                                    {m.access.checking}
                                </>
                            ) : isSetup ? (
                                m.access.createAccess
                            ) : (
                                m.access.continue
                            )}
                        </Button>
                    </form>
                )}
            </CardContent>
        </AuthCardShell>
    );
}
