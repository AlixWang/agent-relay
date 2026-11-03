// Unit tests for the command console's safe message renderer.
//
// Run with: node internal/web/ui/_message-render.test.mjs
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const { renderMessage, messageIsLong } = await import(pathToFileURL(path.join(here, 'js', 'message.js')).href);

// Raw HTML from an agent message must never become live markup.
const xss = renderMessage('<img src=x onerror="alert(1)"> & "quoted"');
assert.ok(xss.includes('&lt;img src=x onerror=&quot;alert(1)&quot;&gt;'), 'raw HTML must be escaped: ' + xss);
assert.ok(!xss.includes('<img'), 'the renderer must not emit a raw img tag');
assert.ok(xss.includes('&amp;'), 'ampersands must be escaped');

// Paragraphs keep explicit line breaks but blank lines become block separators.
assert.equal(
  renderMessage('第一行\n第二行\n\n第二段'),
  '<p>第一行<br>第二行</p><p>第二段</p>',
  'paragraph and line-break layout'
);

// A report bullet gets a scannable bold label before the colon.
assert.equal(
  renderMessage('- fe (seq 654): tick 暂停\n- Lisa (seq 655): 40 分钟'),
  '<ul><li><strong class="msg-li-label">fe (seq 654):</strong> tick 暂停</li>' +
    '<li><strong class="msg-li-label">Lisa (seq 655):</strong> 40 分钟</li></ul>',
  'bullet labels'
);

// Chinese section headings and the closing conclusion get their own blocks.
assert.equal(
  renderMessage('一、hook 同病检查回执\n\n共同结论：无人命中'),
  '<h4 class="msg-heading msg-section">一、hook 同病检查回执</h4>' +
    '<div class="msg-summary"><span class="msg-summary-k">共同结论：</span> 无人命中</div>',
  'section heading and summary callout'
);

// Markdown inline formatting and safe links.
const inline = renderMessage('**粗体** `code <x>` [链接](https://example.com/a?x=1&y=2)');
assert.ok(inline.includes('<strong>粗体</strong>'), 'bold: ' + inline);
assert.ok(inline.includes('<code>code &lt;x&gt;</code>'), 'inline code must be escaped: ' + inline);
assert.ok(
  inline.includes('<a href="https://example.com/a?x=1&amp;y=2" target="_blank" rel="noopener noreferrer">链接</a>'),
  'safe link: ' + inline
);

// Unsafe schemes stay text, not clickable anchors.
const unsafe = renderMessage('[x](javascript:alert(1))');
assert.ok(!unsafe.includes('<a '), 'javascript: links must not become anchors: ' + unsafe);
assert.ok(unsafe.includes('[x](javascript:alert(1))'), 'unsafe link text should survive: ' + unsafe);

// Fenced code is preserved and escaped.
const code = renderMessage('```go\nif a < b { return }\n```');
assert.ok(code.includes('<pre class="msg-code"><code>if a &lt; b { return }</code></pre>'), 'fenced code: ' + code);

// Ordered lists and blockquotes.
assert.equal(
  renderMessage('1. 第一步\n2. 第二步'),
  '<ol><li>第一步</li><li>第二步</li></ol>',
  'ordered list'
);
assert.equal(
  renderMessage('> 注意\n> 第二行'),
  '<blockquote><p>注意<br>第二行</p></blockquote>',
  'blockquote'
);

// Collapse threshold: normal chat is never hidden; a long report is.
assert.equal(messageIsLong('收到，马上处理'), false, 'short chat must not collapse');
assert.equal(messageIsLong('x'.repeat(1200)), true, 'long text must collapse');
assert.equal(messageIsLong(Array(16).fill('一行').join('\n')), true, 'many lines must collapse');

console.log('message renderer tests passed');
