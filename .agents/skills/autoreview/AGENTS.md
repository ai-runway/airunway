# Autoreview Skill

- Canonical source: `openclaw/agent-skills`, under `skills/autoreview`.
- Before editing any copy, fast-forward a checkout of `openclaw/agent-skills` from `origin/main`.
- Make and validate shared runtime changes in canonical `skills/autoreview` first, then sync the complete runtime bundle into downstream repos.
- Exclude upstream repository-only regression artifacts (`scripts/autoreview_test.py` and `tests/`) from installable and downstream copies.
- Never create repo-local behavior variants; packaging exclusions do not change skill behavior, and downstream behavioral differences belong in repo-level validation.
