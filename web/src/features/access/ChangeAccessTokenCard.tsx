import { useState } from "react";
import type { FormEvent } from "react";
import { LoaderCircle } from "lucide-react";
import { Field, FormError } from "@/components/forms";
import { Button } from "@/components/ui/button";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { describeError } from "@/lib/errors";

function validTokenLength(token: string): boolean {
    const bytes = new TextEncoder().encode(token).byteLength;
    return bytes >= 16 && bytes <= 128;
}

export function ChangeAccessTokenCard({
    onCancel,
    onSubmit,
}: {
    onCancel: () => void;
    onSubmit: (current: string, next: string) => Promise<void>;
}) {
    const [current, setCurrent] = useState("");
    const [next, setNext] = useState("");
    const [confirmation, setConfirmation] = useState("");
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState("");

    const clearFields = () => {
        setCurrent("");
        setNext("");
        setConfirmation("");
    };

    const submit = async (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const submittedCurrent = current.trim();
        const submittedNext = next.trim();
        const submittedConfirmation = confirmation.trim();
        clearFields();

        if (!validTokenLength(submittedCurrent)) {
            setError("Enter the current access token.");
            return;
        }
        if (!validTokenLength(submittedNext)) {
            setError("The new access token must be 16–128 bytes.");
            return;
        }
        if (submittedNext !== submittedConfirmation) {
            setError("The new access tokens do not match.");
            return;
        }
        if (submittedNext === submittedCurrent) {
            setError("Choose a new access token that differs from the current one.");
            return;
        }

        setSubmitting(true);
        setError("");
        try {
            await onSubmit(submittedCurrent, submittedNext);
        } catch (requestError) {
            setError(describeError(requestError));
        } finally {
            setSubmitting(false);
        }
    };

    return (
        <Card className="panel">
            <CardHeader>
                <CardTitle>Change access token</CardTitle>
                <CardDescription>
                    This signs out every other browser and changes the
                    subscription URL, every SOCKS credential and the smart
                    proxy address. Copy the new subscription URL into each
                    client afterwards.
                </CardDescription>
            </CardHeader>
            <CardContent>
                <form className="grid gap-4 sm:max-w-md" noValidate onSubmit={submit}>
                    <FormError message={error} />
                    <Field label="Current access token" name="current-access-token">
                        <Input
                            autoComplete="off"
                            id="current-access-token"
                            name="current-access-token"
                            onChange={(event) => setCurrent(event.currentTarget.value)}
                            type="password"
                            value={current}
                        />
                    </Field>
                    <Field label="New access token" name="new-access-token">
                        <Input
                            autoComplete="off"
                            id="new-access-token"
                            name="new-access-token"
                            onChange={(event) => setNext(event.currentTarget.value)}
                            type="password"
                            value={next}
                        />
                    </Field>
                    <Field
                        label="Confirm new access token"
                        name="new-access-token-confirmation"
                    >
                        <Input
                            autoComplete="off"
                            id="new-access-token-confirmation"
                            name="new-access-token-confirmation"
                            onChange={(event) =>
                                setConfirmation(event.currentTarget.value)
                            }
                            type="password"
                            value={confirmation}
                        />
                    </Field>
                    <div className="flex gap-2">
                        <Button disabled={submitting} type="submit">
                            {submitting ? (
                                <>
                                    <LoaderCircle className="size-4 animate-spin" />
                                    Changing
                                </>
                            ) : (
                                "Save new token"
                            )}
                        </Button>
                        <Button
                            disabled={submitting}
                            onClick={onCancel}
                            type="button"
                            variant="outline"
                        >
                            Cancel
                        </Button>
                    </div>
                </form>
            </CardContent>
        </Card>
    );
}
