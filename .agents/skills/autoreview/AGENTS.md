# Autoreview Skill

- Canonical source: `openclaw/agent-skills`, under `skills/autoreview`.
- Before editing any copy, fast-forward a checkout of `openclaw/agent-skills` from `origin/main`.
- Make and validate shared runtime changes in canonical `skills/autoreview` first, then sync the complete runtime bundle into downstream repos.
- Exclude upstream repository-only regression artifacts (`scripts/autoreview_test.py` and `tests/`) from installable and downstream copies.
- Never create *undocumented* repo-local behavior variants. The only permitted downstream deviations are the ones listed in `README.md` under "Local differences"; preserve those across syncs rather than reverting them to upstream defaults. Adding a new one requires recording it in both `README.md` and `SKILL.md` in the same change.
- Packaging exclusions do not change skill behavior, and any other downstream behavioral difference belongs in repo-level validation.
