import { useState } from "react";
import type { FormEvent } from "react";
import { LoaderCircle } from "lucide-react";
import type { StatusResponse } from "@/types";
import { Field, FormError, selectClassName } from "@/components/forms";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useI18n } from "@/i18n";
import { describeError } from "@/lib/errors";
import { fieldLabel, providerLabel } from "./providers";

export function ProviderCredentialsForm({
    error,
    fixedProvider,
    idPrefix,
    onSubmit,
    providers = [],
    providerAuth,
}: {
    error?: string;
    fixedProvider?: string;
    idPrefix: string;
    onSubmit: (
        provider: string,
        fields: Record<string, string>,
    ) => Promise<void>;
    providers?: string[];
    providerAuth: StatusResponse["providerAuth"];
}) {
    const { m } = useI18n();
    const [values, setValues] = useState<Record<string, string>>({});
    const [provider, setProvider] = useState(fixedProvider ?? "");
    const [submitting, setSubmitting] = useState(false);
    const [localError, setLocalError] = useState("");
    const selectedProvider = fixedProvider ?? provider;
    const authSpec = providerAuth[selectedProvider];
    const fields = authSpec?.fields ?? [];
    const providerID = `${idPrefix}-provider`;

    const submit = async (event: FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const submittedFields = Object.fromEntries(
            fields.map(({ key, secret }) => [
                key,
                secret ? values[key] ?? "" : (values[key] ?? "").trim(),
            ]),
        );
        setValues((current) =>
            Object.fromEntries(
                fields.map(({ key, secret }) => [
                    key,
                    secret ? "" : current[key] ?? "",
                ]),
            ),
        );
        if (!selectedProvider || !authSpec) {
            setLocalError(m.providers.form.chooseError);
            return;
        }
        if (fields.some(({ key }) => !submittedFields[key])) {
            setLocalError(m.providers.form.fillAll);
            return;
        }
        setSubmitting(true);
        setLocalError("");
        try {
            await onSubmit(selectedProvider, submittedFields);
            setValues({});
        } catch (requestError) {
            setLocalError(describeError(requestError));
        } finally {
            setValues((current) =>
                Object.fromEntries(
                    fields.map(({ key, secret }) => [
                        key,
                        secret ? "" : current[key] ?? "",
                    ]),
                ),
            );
            setSubmitting(false);
        }
    };

    return (
        <form className="grid gap-4" noValidate onSubmit={submit}>
            <FormError message={localError || error || ""} />
            {fixedProvider ? null : (
                <Field label={m.providers.form.provider} name={providerID}>
                    <select
                        className={selectClassName}
                        id={providerID}
                        name="provider"
                        onChange={(event) => {
                            setProvider(event.currentTarget.value);
                            setValues({});
                            setLocalError("");
                        }}
                        value={provider}
                    >
                        <option value="">{m.providers.form.choose}</option>
                        {providers.map((id) => (
                            <option key={id} value={id}>
                                {providerLabel(id)}
                            </option>
                        ))}
                    </select>
                </Field>
            )}
            {fields.map((field) => {
                const fieldID = `${idPrefix}-${field.key}`;
                return (
                    <Field
                        key={field.key}
                        label={fieldLabel(field.label)}
                        name={fieldID}
                    >
                        <Input
                            autoComplete="off"
                            id={fieldID}
                            name={field.key}
                            onChange={(event) =>
                                setValues({
                                    ...values,
                                    [field.key]: event.currentTarget.value,
                                })
                            }
                            type={field.secret ? "password" : "text"}
                            value={values[field.key] ?? ""}
                        />
                    </Field>
                );
            })}
            {authSpec && fields.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                    {m.providers.form.noSignIn}
                </p>
            ) : null}
            <Button className="mt-2 w-full" disabled={submitting} type="submit">
                {submitting ? (
                    <>
                        <LoaderCircle className="size-4 animate-spin" />
                        {m.providers.form.adding}
                    </>
                ) : fixedProvider ? (
                    m.providers.add(providerLabel(fixedProvider))
                ) : (
                    m.providers.form.submit
                )}
            </Button>
        </form>
    );
}
