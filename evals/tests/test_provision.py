"""Provisioning contract tests. No server, no database, no docker: the command is a tiny Python script, so
every branch of the contract is exercised keylessly."""

from __future__ import annotations

import sys

import pytest

from longmemeval.provision import KEY_PREFIX, NAME_PLACEHOLDER, ProvisionError, parse_command, provision_project


def _script(body: str) -> list[str]:
    """An argv that runs `body` as a Python program, standing in for the operator's provision command."""
    return [sys.executable, "-c", body, NAME_PLACEHOLDER]


def test_returns_the_key_printed_on_stdout() -> None:
    argv = _script(f"import sys; print({KEY_PREFIX!r} + sys.argv[1])")
    assert provision_project(argv, "lme-7") == f"{KEY_PREFIX}lme-7"


def test_substitutes_the_name_per_call() -> None:
    """Each ingestion must get its OWN project, so the name has to reach the command — a template whose
    placeholder was ignored would provision one project and silently restore the shared-project mode."""
    argv = _script(f"import sys; print({KEY_PREFIX!r} + sys.argv[1])")
    assert provision_project(argv, "a") != provision_project(argv, "b")


def test_stderr_chatter_is_ignored() -> None:
    """`lore provision` prints the project id and a warning to stderr and only the token to stdout. Reading
    stdout alone is what makes that contract usable; a parser that merged the streams would pick up the
    warning as the key."""
    argv = _script(f"import sys; print('provisioned project abc', file=sys.stderr); print({KEY_PREFIX!r} + 'k')")
    assert provision_project(argv, "n") == f"{KEY_PREFIX}k"


def test_non_key_first_line_fails_loudly_and_redacted() -> None:
    """A template that still passes --out prints a success line instead of the token. That must fail here,
    not later as an authentication error on every request — and the offending line is quoted only as a short
    preview, because a parser cannot be sure the line it rejected was not itself a secret."""
    secret_ish = "x" * 60
    argv = _script(f"print({secret_ish!r})")
    with pytest.raises(ProvisionError) as exc:
        provision_project(argv, "n")
    assert "first line" in str(exc.value)
    assert secret_ish not in str(exc.value), "a rejected line must never be reproduced in full"


def test_empty_stdout_fails() -> None:
    argv = _script("pass")
    with pytest.raises(ProvisionError, match="printed nothing"):
        provision_project(argv, "n")


def test_non_zero_exit_surfaces_stderr() -> None:
    argv = _script("import sys; print('database unreachable', file=sys.stderr); sys.exit(3)")
    with pytest.raises(ProvisionError, match="database unreachable"):
        provision_project(argv, "n")


def test_missing_executable_fails_clearly() -> None:
    with pytest.raises(ProvisionError, match="not found"):
        provision_project(["definitely-not-a-real-command-xyz", NAME_PLACEHOLDER], "n")


def test_parse_command_requires_the_placeholder() -> None:
    """Without {name} every ingestion provisions the same project — the contamination this exists to stop,
    wearing the costume of a working configuration."""
    with pytest.raises(ProvisionError, match="placeholder"):
        parse_command("lore provision --project fixed")


def test_parse_command_rejects_empty() -> None:
    with pytest.raises(ProvisionError, match="empty"):
        parse_command("   ")


def test_parse_command_splits_like_a_shell() -> None:
    argv = parse_command('docker run --rm img provision --project "{name}"')
    assert argv == ["docker", "run", "--rm", "img", "provision", "--project", "{name}"]
