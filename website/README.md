# AI Runway website

This is the source for the AI Runway documentation site published at
[**ai-runway.github.io/airunway**](https://ai-runway.github.io/airunway/).
Built with [Docusaurus](https://docusaurus.io/) 3.

## Content layout

The site renders content from two places:

- `/website/src/pages/` — landing page and other React pages
- `/docs/*.md` (at the repo root) — every documentation page

The docs plugin is configured with `docs.path: '../docs'` so the markdown
files double as in-repo docs (viewable on GitHub) and as the website's
content. **Write docs as plain GitHub-Flavored Markdown.** Docusaurus is set
to `markdown.format: 'detect'`, which means `.md` files are NOT treated as
MDX — content like `{name}` or `<pod-name>` renders verbatim. If you want
JSX, rename the file to `.mdx`.

When you add a new doc, also add it to [`sidebars.js`](./sidebars.js).

## Local development

Requires [Bun](https://bun.sh/) (matches the rest of the repo).

```bash
bun install      # one-time
bun run start    # http://localhost:3000/airunway/ with hot reload
bun run build    # what CI runs; must pass before merge
bun run serve    # serve the production build locally on :3000
```

## Deployment

`.github/workflows/deploy-docs.yml` builds the site and publishes it via the
GitHub Actions Pages flow (`actions/upload-pages-artifact` →
`actions/deploy-pages`). The site is deployed on every push to `main` on the
canonical `ai-runway/airunway` repo. Pull requests and forks build the
site to verify it compiles, but only the canonical repo deploys.

First-time setup (needs a repository admin): **Settings → Pages → Build and
deployment → Source = "GitHub Actions"**. No `gh-pages` branch is involved —
the artifact is served directly from the workflow.
