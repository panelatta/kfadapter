import * as React from "react";

import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "flex h-10 w-full min-w-0 rounded-md border border-input bg-card px-3 py-1 text-base text-foreground shadow-xs outline-none transition-[color,border-color,box-shadow] placeholder:text-muted-foreground/80 selection:bg-primary selection:text-primary-foreground disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:h-9 md:text-sm focus-visible:border-ring focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/20 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
