import { createMarkdownProcessor } from '@astrojs/markdown-remark';
import rehypeRaw from 'rehype-raw';

let links;
const renderer = await createMarkdownProcessor({
  syntaxHighlight: false,
  rehypePlugins: [
    () => (tree, file) => {
      function collect(node) {
        if (node.type === 'element') {
          const url = node.tagName === 'a' ? node.properties.href
            : node.tagName === 'img' ? node.properties.src : undefined;
          if (typeof url === 'string') {
            links.push({ url, line: node.position?.start.line ?? 1 });
          }
        }
        for (const child of node.children ?? []) collect(child);
      }
      // Inspect raw HTML without changing when Astro assigns heading IDs.
      collect(rehypeRaw()(tree, file));
    },
  ],
});

let input = '';
for await (const chunk of process.stdin) input += chunk;
const result = {};
for (const [name, markdown] of Object.entries(JSON.parse(input))) {
  links = [];
  const { code } = await renderer.render(markdown);
  result[name] = { html: code, links };
}
process.stdout.write(JSON.stringify(result));
