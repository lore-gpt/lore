"""Provision one isolated Lore project per ingestion, by running an operator-supplied command.

Lore scopes distilled recall to a PROJECT and only the raw tail to a run, so giving each question its own
run is not isolation: question k's pack can cite memories distilled from questions 1..k-1. Measured on this
harness with three questions in one project, 25-65% of a pack's cited sources came from a different
question's history — and because the pack is a fixed budget, those foreign sources DISPLACE the evidence the
answerer needs rather than merely adding noise. So a comparison against a system that does isolate (mem0
filters by a fresh user_id) has to give each ingestion its own project.

A project can only be created by `lore provision`, which needs the database URL — the harness speaks only
HTTP and has no business holding database credentials or knowing whether the server runs under compose, as a
bare binary, or in a cluster. So the operator supplies the invocation once and this module runs it, which
keeps the harness independent of the deployment shape.

The command's output contract is deliberately narrow: `lore provision` without `--out` prints the token to
stdout and everything else to stderr, precisely so a script can capture just the token. A command that does
not honour that fails loudly here rather than producing a broken run.

The token itself never leaves this module's return value — not into a log, not into the report. The command
TEMPLATE (which carries no secret) is what the report records, because that is the part a later reader needs
in order to reproduce the run.
"""

from __future__ import annotations

import shlex
import subprocess
from collections.abc import Sequence

# Substituted into the command template for each ingestion, so every project gets a distinct name.
NAME_PLACEHOLDER = "{name}"

# Every Lore secret key starts with this. Checking it is what turns "the command printed something" into
# "the command printed a key" — without it, a template that prints a success message (say, one that still
# passes --out) would be silently accepted and every request would then fail with an unusable token.
KEY_PREFIX = "lore_sk_"

# How much of an unexpected first line may be quoted back in an error. Enough to recognise a "provisioned
# project ...; wrote credentials to ..." line and diagnose the mistake, short enough that a real token —
# which is this prefix plus 43 random characters — is never reproduced in full.
_PREVIEW_CHARS = 12


class ProvisionError(RuntimeError):
    """The provision command failed, or did not print a key the way the contract requires."""


def parse_command(template: str) -> list[str]:
    """Split a command template into argv. Raises ProvisionError when it is empty or has no {name} slot.

    Requiring the placeholder is not pedantry: a template without it provisions the SAME project for every
    ingestion, which is exactly the shared-project mode this module exists to replace — and it would look
    like it was working."""
    argv = shlex.split(template)
    if not argv:
        raise ProvisionError("the provision command is empty")
    if not any(NAME_PLACEHOLDER in part for part in argv):
        raise ProvisionError(
            f"the provision command has no {NAME_PLACEHOLDER} placeholder, so every ingestion would land in "
            "the same project — which is the contamination this isolates against"
        )
    return argv


def provision_project(argv: Sequence[str], name: str, *, timeout: float = 180.0) -> str:
    """Run the provision command for one project called `name` and return its API key.

    The command runs without a shell, and `name` is substituted after the template is split, so a project
    name can never turn into extra arguments."""
    resolved = [part.replace(NAME_PLACEHOLDER, name) for part in argv]
    try:
        proc = subprocess.run(resolved, capture_output=True, text=True, timeout=timeout, check=False)
    except FileNotFoundError as exc:
        raise ProvisionError(f"provision command not found: {resolved[0]!r}") from exc
    except subprocess.TimeoutExpired as exc:
        raise ProvisionError(f"provision command timed out after {timeout:g}s") from exc

    if proc.returncode != 0:
        # stderr is safe to surface: `lore provision` puts the project id and its warnings there and keeps
        # the token on stdout.
        raise ProvisionError(
            f"provision command failed (exit {proc.returncode}): {proc.stderr.strip() or '<no stderr>'}"
        )

    for line in proc.stdout.splitlines():
        candidate = line.strip()
        if not candidate:
            continue
        if candidate.startswith(KEY_PREFIX):
            return candidate
        # Redacted on purpose. If this line were a token after all, quoting it in an exception would put it
        # into whatever caught the exception — a log, a CI transcript, a bug report.
        raise ProvisionError(
            f"provision command must print the API key as the first line of stdout, but printed "
            f"{candidate[:_PREVIEW_CHARS]!r}... ({len(candidate)} chars); `lore provision` does this when "
            "run WITHOUT --out (which diverts the token to a file)"
        )
    raise ProvisionError("provision command printed nothing on stdout; it must print the API key there")
