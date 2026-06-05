// @ts-check
// AI Runway docs sidebar.
//
// Source of truth for the documentation table is /agents.md; keep this in sync
// when adding new docs files to /docs.

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  mainSidebar: [
    {
      type: 'category',
      label: 'Getting Started',
      collapsed: false,
      items: ['architecture', 'development'],
    },
    {
      type: 'category',
      label: 'Architecture',
      collapsed: false,
      items: [
        'controller-architecture',
        'web-ui-architecture',
        'design-decisions',
      ],
    },
    {
      type: 'category',
      label: 'CRDs & API',
      collapsed: false,
      items: ['crd-reference', 'api', 'providers'],
    },
    {
      type: 'category',
      label: 'Operations',
      collapsed: false,
      items: [
        'gateway',
        'observability',
        'azure-autoscaling',
        'csi-azure-lustre',
        'headlamp-plugin',
        'versioning-upgrades',
      ],
    },
    {
      type: 'category',
      label: 'Contributing',
      collapsed: false,
      items: ['standards'],
    },
  ],
};

export default sidebars;
