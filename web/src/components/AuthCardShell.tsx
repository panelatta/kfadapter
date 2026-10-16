import type { ReactNode } from "react";
import { BrandMark } from "@/components/BrandMark";
import { LanguageSwitch } from "@/components/LanguageSwitch";
import { Card } from "@/components/ui/card";

export function AuthCardShell({ children }: { children: ReactNode }) {
    return (
        <main className="flex min-h-screen flex-col bg-background px-4 py-6 text-foreground sm:px-6">
            <div className="flex items-center justify-between gap-4">
                <BrandMark />
                <LanguageSwitch />
            </div>
            <div className="flex flex-1 items-center justify-center py-10">
                <Card className="w-full max-w-md gap-6 py-7 shadow-[0_1px_2px_rgb(0_0_0/0.04),0_16px_40px_-24px_rgb(0_0_0/0.25)]">
                    {children}
                </Card>
            </div>
        </main>
    );
}
