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
import { useI18n } from "@/i18n";
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
    const { m } = useI18n();
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
            setError(m.changeToken.enterCurrent);
            return;
        }
        if (!validTokenLength(submittedNext)) {
            setError(m.changeToken.nextInvalid);
            return;
        }
        if (submittedNext !== submittedConfirmation) {
            setError(m.changeToken.mismatch);
            return;
        }
        if (submittedNext === submittedCurrent) {
            setError(m.changeToken.unchanged);
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
        <Card>
            <CardHeader>
                <CardTitle>{m.changeToken.title}</CardTitle>
                <CardDescription>
                    {m.changeToken.description}
                </CardDescription>
            </CardHeader>
            <CardContent>
                <form className="grid gap-4 sm:max-w-md" noValidate onSubmit={submit}>
                    <FormError message={error} />
                    <Field label={m.changeToken.current} name="current-access-token">
                        <Input
                            autoComplete="off"
                            id="current-access-token"
                            name="current-access-token"
                            onChange={(event) => setCurrent(event.currentTarget.value)}
                            type="password"
                            value={current}
                        />
                    </Field>
                    <Field label={m.changeToken.next} name="new-access-token">
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
                        label={m.changeToken.confirm}
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
                    <div className="flex gap-2 pt-1">
                        <Button disabled={submitting} type="submit">
                            {submitting ? (
                                <>
                                    <LoaderCircle className="size-4 animate-spin" />
                                    {m.changeToken.saving}
                                </>
                            ) : (
                                m.changeToken.save
                            )}
                        </Button>
                        <Button
                            disabled={submitting}
                            onClick={onCancel}
                            type="button"
                            variant="outline"
                        >
                            {m.changeToken.cancel}
                        </Button>
                    </div>
                </form>
            </CardContent>
        </Card>
    );
}
