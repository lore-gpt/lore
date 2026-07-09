# Security Policy

## Reporting a vulnerability

**Please do not report security issues through public GitHub issues, discussions, or pull requests.**

Instead, email **support@loregpt.ai** with:

- a description of the issue and its impact,
- steps to reproduce (proof-of-concept if possible),
- affected version/commit and environment.

You'll get an acknowledgement within **3 business days** and a triage assessment within **7 days**. We
practice coordinated disclosure: we'll work with you on a fix and a disclosure timeline, and credit you
(unless you prefer to remain anonymous).

## Supported versions

Lore is pre-release (`0.x`). Until `v1.0`, only the latest release/`main` receives security fixes.

## Scope notes

Lore is shared memory for teams of agents, so it has a security surface most memory tools don't:

- **Memory poisoning** is a first-class threat, not an afterthought. A compromised or buggy agent must
  not be able to steer other agents by writing instruction-shaped memories. Lore's defenses: memory
  content is always packaged **as data** (packs mark the data/instruction boundary), new agent identities
  start in a `quarantine` trust tier, provenance is mandatory, and ACL defaults to the narrowest scope.
- **Tenant isolation** is enforced in the query layer (every query is `project_id`-scoped; the data
  access layer will not compile a query without it) with row-level security behind it.

If you find a way around any of these, that's exactly the kind of report we want.
