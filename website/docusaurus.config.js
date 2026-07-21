// @ts-check
// AI Runway website — Docusaurus 3 classic preset.
// Reference: https://docusaurus.io/docs/api/docusaurus-config

import {themes as prismThemes} from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'AI Runway',
  tagline: 'Deploy and manage large language models on Kubernetes — no YAML required.',
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
  },

  url: 'https://kaito-project.github.io',
  baseUrl: '/airunway/',
  trailingSlash: false,

  organizationName: 'kaito-project',
  projectName: 'airunway',

  // Strict by default — broken links should fail CI, not slip into prod.
  onBrokenLinks: 'throw',

  markdown: {
    // Treat .md as plain GitHub-Flavored Markdown and .mdx as MDX. The /docs
    // tree is shared with the GitHub view, where contributors write GFM and
    // occasionally include text like {name} or <pod-name> that MDX would try
    // to evaluate as JSX. `detect` keeps that content rendering safely.
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          // Use the repo-root /docs as the single source of truth so that
          // markdown viewed on GitHub and rendered on the website come from
          // the same files. Versioned snapshots still live under
          // website/versioned_docs once we cut a release.
          path: '../docs',
          sidebarPath: './sidebars.js',
          routeBasePath: '/',
          // Internal planning/design docs under docs/plans/ are kept in the repo
          // for contributors but must not be published as public site pages.
          exclude: ['plans/**'],
          // Use the function form so the relative `../docs` source path is
          // rewritten to a proper `docs/<file>` URL on github.com.
          editUrl: ({docPath}) =>
            `https://github.com/kaito-project/airunway/edit/main/docs/${docPath}`,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      // Social card asset will be added in a follow-up; until then, fall back
      // to the default Open Graph image rendering.
      colorMode: {
        defaultMode: 'dark',
        respectPrefersColorScheme: true,
      },
      navbar: {
        title: 'AI Runway',
        logo: {
          alt: 'AI Runway Logo',
          src: 'img/logo.png',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'mainSidebar',
            position: 'left',
            label: 'Docs',
          },
          {
            href: 'https://github.com/kaito-project/airunway/releases',
            label: 'Releases',
            position: 'left',
          },
          {
            href: 'https://github.com/kaito-project/airunway',
            position: 'right',
            className: 'header-github-link',
            'aria-label': 'GitHub repository',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              {label: 'Introduction', to: '/architecture'},
              {label: 'Quick Start', to: '/development'},
              {label: 'CRD Reference', to: '/crd-reference'},
              {label: 'Providers', to: '/providers'},
            ],
          },
          {
            title: 'Community',
            items: [
              {
                label: 'GitHub',
                href: 'https://github.com/kaito-project/airunway',
              },
              {
                label: 'Issues',
                href: 'https://github.com/kaito-project/airunway/issues',
              },
              {
                label: 'Discussions',
                href: 'https://github.com/kaito-project/airunway/discussions',
              },
            ],
          },
          {
            title: 'More',
            items: [
              {
                label: 'KAITO project',
                href: 'https://github.com/kaito-project/kaito',
              },
              {
                label: 'Headlamp',
                href: 'https://headlamp.dev/',
              },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} The AI Runway Authors. Built with Docusaurus.`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ['bash', 'yaml', 'json', 'go', 'docker'],
      },
    }),
};

export default config;
