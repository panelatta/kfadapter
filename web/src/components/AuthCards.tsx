import { useState, type FormEvent, type ReactNode } from "react";
import { LoaderCircle, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { providerLabel } from "@/lib/console";
import { describeError } from "@/lib/errors";

export const selectClassName =
    "flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-xs outline-none transition-[color,box-shadow] focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50";

function AuthCardShell({ children }: { children: ReactNode }) {
    return (
        <main className="flex min-h-screen items-center justify-center bg-background px-4 py-10 text-foreground">
            <div className="boot w-full max-w-md">
                <div className="mb-5 flex items-end justify-between gap-4 px-1">
                    <div>
                        <p className="glow font-sans text-lg font-bold uppercase tracking-[0.34em] text-primary">
                            Kfadapter
                        </p>
                        <p className="mt-1 text-[0.625rem] uppercase tracking-[0.24em] text-muted-foreground">
                            Local control plane
                        </p>
                    </div>
                    <div
                        aria-hidden="true"
                        className="mb-1.5 flex items-center gap-1.5"
                    >
                        <span className="led led--ok" />
                        <span className="led led--warn led--pulse" />
                        <span className="led" />
                    </div>
                </div>
                <Card className="panel w-full">
                    {children}
                    <CardFooter className="justify-between border-t border-dashed border-border/70 pt-4 text-[0.625rem] uppercase tracking-[0.22em] text-muted-foreground [&.border-t]:pt-4">
                        <span>Console access</span>
                        <span aria-hidden="true">▪ ▪ ▪</span>
                    </CardFooter>
                </Card>
            </div>
        </main>
    );
}

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

export function LoadingCard({
    title = "Opening kfadapter",
    description = "Checking console access.",
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
                <CardDescription>{description}</CardDescription>
            </CardHeader>
        </AuthCardShell>
    );
}

export function AccessTokenCard({
    mode,
    onSubmit,
    error,
    setupAllowed = true,
}: {
    mode: "setup" | "login";
    onSubmit: (token: string) => Promise<void>;
    error: string;
    setupAllowed?: boolean;
}) {
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
            setLocalError("That access token is too short or too long.");
            return;
        }
        if (isSetup && submittedToken !== submittedConfirmation) {
            setLocalError("The access tokens do not match.");
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
                <h1 className="font-sans text-xl font-semibold uppercase tracking-[0.05em]">
                    {isSetup ? "Create console access" : "Unlock console"}
                </h1>
                <CardDescription>
                    {isSetup
                        ? "Set an access token for this console."
                        : "Enter your access token to continue."}
                </CardDescription>
            </CardHeader>
            <CardContent>
                {isSetup && !setupAllowed ? (
                    <p className="text-sm" role="alert">
                        For safety, the first access token can only be created
                        from the device running kfadapter. Open this console at
                        http://127.0.0.1 on that device, for example through an
                        SSH port forward, then return here to unlock it.
                    </p>
                ) : (
                <form className="grid gap-4" noValidate onSubmit={submit}>
                    <FormError message={localError || error} />
                    <Field label="Access token" name="access-token">
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
                            label="Confirm access token"
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
                                Checking
                            </>
                        ) : isSetup ? (
                            "Create access"
                        ) : (
                            "Continue"
                        )}
                    </Button>
                </form>
                )}
            </CardContent>
        </AuthCardShell>
    );
}

export function ProviderLoginCard({
    onSubmit,
    error,
    providers,
}: {
    onSubmit: (
        provider: string,
        account: string,
        password: string,
    ) => Promise<void>;
    error: string;
    providers: string[];
}) {
    return (
        <AuthCardShell>
            <CardHeader>
                <h1 className="font-sans text-xl font-semibold uppercase tracking-[0.05em]">
                    Connect account
                </h1>
                <CardDescription>
                    Choose a provider before entering account credentials.
                </CardDescription>
            </CardHeader>
            <CardContent>
                <ProviderCredentialsForm
                    error={error}
                    idPrefix="initial"
                    onSubmit={onSubmit}
                    providers={providers}
                />
            </CardContent>
        </AuthCardShell>
    );
}

export function ProviderCredentialsForm({
    error,
    fixedProvider,
    idPrefix,
    onSubmit,
    providers = [],
}: {
    error?: string;
    fixedProvider?: string;
    idPrefix: string;
    onSubmit: (
        provider: string,
        account: string,
        password: string,
    ) => Promise<void>;
    providers?: string[];
}) {
    const [account, setAccount] = useState("");
    const [password, setPassword] = useState("");
    const [provider, setProvider] = useState(fixedProvider ?? "");
    const [submitting, setSubmitting] = useState(false);
    const [localError, setLocalError] = useState("");
    const accountID = `${idPrefix}-account`;
    const passwordID = `${idPrefix}-password`;
    const providerID = `${idPrefix}-provider`;

    const submit = async (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const selectedProvider = fixedProvider ?? provider;
        const email = account.trim();
        const submittedPassword = password;
        setPassword("");
        if (!selectedProvider) {
            setLocalError(
                "Choose a provider before entering account credentials.",
            );
            return;
        }
        if (!email || !submittedPassword) {
            setLocalError("Enter your email and password.");
            return;
        }
        setSubmitting(true);
        setLocalError("");
        try {
            await onSubmit(selectedProvider, email, submittedPassword);
            setAccount("");
        } catch (requestError) {
            setLocalError(describeError(requestError));
        } finally {
            setPassword("");
            setSubmitting(false);
        }
    };

    return (
        <form className="grid gap-4" noValidate onSubmit={submit}>
            <FormError message={localError || error || ""} />
            {fixedProvider ? null : (
                <Field label="Provider" name={providerID}>
                    <select
                        className={selectClassName}
                        id={providerID}
                        name="provider"
                        onChange={(event) =>
                            setProvider(event.currentTarget.value)
                        }
                        value={provider}
                    >
                        <option value="">Choose provider…</option>
                        {providers.map((id) => (
                            <option key={id} value={id}>
                                {providerLabel(id)}
                            </option>
                        ))}
                    </select>
                </Field>
            )}
            <Field label="Email" name={accountID}>
                <Input
                    autoComplete="off"
                    id={accountID}
                    inputMode="email"
                    name="account"
                    onChange={(event) => setAccount(event.currentTarget.value)}
                    type="email"
                    value={account}
                />
            </Field>
            <Field label="Password" name={passwordID}>
                <Input
                    autoComplete="off"
                    id={passwordID}
                    name="password"
                    onChange={(event) => setPassword(event.currentTarget.value)}
                    type="password"
                    value={password}
                />
            </Field>
            <Button className="mt-2 w-full" disabled={submitting} type="submit">
                {submitting ? (
                    <>
                        <LoaderCircle className="size-4 animate-spin" />
                        Signing in
                    </>
                ) : fixedProvider ? (
                    `Connect ${providerLabel(fixedProvider)}`
                ) : (
                    "Sign in"
                )}
            </Button>
        </form>
    );
}

export function Field({
    label,
    name,
    children,
}: {
    label: string;
    name: string;
    children: ReactNode;
}) {
    return (
        <label className="grid gap-2" htmlFor={name}>
            <span className="text-[0.65rem] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
                {label}
            </span>
            {children}
        </label>
    );
}

export function FormError({ message }: { message: string }) {
    return message ? (
        <p
            className="flex items-start gap-2.5 rounded-sm border border-destructive/35 bg-destructive/10 px-3 py-2.5 text-[0.8125rem] leading-5 text-destructive"
            role="alert"
        >
            <span
                aria-hidden="true"
                className="led led--err led--pulse mt-1.5"
            />
            <span className="min-w-0">{message}</span>
        </p>
    ) : null;
}
