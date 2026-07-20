"use client";

import { useState } from "react";

import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// Copies text to the clipboard with a brief confirmation. Fails silently when the
// clipboard API is unavailable (e.g. a non-secure context) rather than throwing.
export function CopyButton({
  value,
  label = "Copy",
  className,
}: {
  value: string;
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  // An empty label renders icon-only; keep the button accessible regardless.
  const ariaLabel = label.length > 0 ? label : "Copy";

  function onCopy() {
    // The Clipboard API is undefined in a non-secure context (plain HTTP off
    // localhost) — guard before use so the click never throws.
    if (!navigator.clipboard?.writeText) {
      return;
    }
    navigator.clipboard
      .writeText(value)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      })
      .catch(() => {
        // Write rejected — no-op.
      });
  }

  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      onClick={onCopy}
      aria-label={ariaLabel}
      className={cn("h-7 gap-1.5 px-2 text-muted-foreground", className)}
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
      {label ? <span className="text-xs">{copied ? "Copied" : label}</span> : null}
    </Button>
  );
}
