# chord-gateway docs site

Starlight (Astro) site that renders the public `chord-gateway` Markdown docs.

## Quick start

```bash
cd website
npm install
npm run dev      # http://localhost:4321/chord-gateway/
```

Use Node 22.12+ for local development and CI. `npm run dev` automatically runs `npm run sync` first, and `npm run build` syncs the source Markdown before producing the static site. The sync step converts source Markdown from `../QUICKSTART*.md` and `../docs/*.md` into Starlight content under `src/content/docs/`.

Synced Markdown output is gitignored. Do not edit generated `src/content/docs/*.md` files directly; edit the source files in the repository root or `../docs/` and rerun sync.

## Layout

```text
website/
├── astro.config.mjs        # Starlight config: sidebar, locales, base URL
├── package.json
├── tsconfig.json
├── src/
│   ├── content.config.ts   # Astro content collection definition
│   ├── content/docs/
│   │   ├── index.mdx       # Hand-written English landing page
│   │   ├── zh/index.mdx    # Hand-written Chinese landing page
│   │   └── *.md            # SYNCED — do not edit by hand
│   └── styles/custom.css   # Theme tweaks
└── dist/                   # Build output, gitignored
```

## Build for production

```bash
npm run build
npm run preview
```

The CI workflow at `.github/workflows/docs.yml` runs the same build step and deploys `website/dist/` to GitHub Pages from `main`.
