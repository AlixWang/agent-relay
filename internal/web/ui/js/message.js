// Safe Markdown-lite renderer for command-console messages.
//
// Agent reports arrive as one plain-text payload. Rendering the whole payload
// as a single pre-wrap block turns a long report into a wall of text, so this
// module recognises the small formatting subset agents actually use:
//
//   - paragraphs and explicit line breaks
//   - Markdown headings (#, ##, ...) and Chinese "一、" section headings
//   - bullet / ordered lists, with a bold label before the first colon
//   - blockquotes, fenced code blocks and inline code
//   - pipe tables with optional column alignment
//   - bold / italic / safe (http, https, mailto) links
//
// Raw HTML is never passed through: every text run is escaped before the
// renderer adds its own tags.

import { esc } from './utils.js';

const SAFE_LINK = /^(https?:\/\/|mailto:)/i;

function inline(text) {
  let out = '';
  let i = 0;

  while (i < text.length) {
    const ch = text[i];

    // Inline code: `...`
    if (ch === '`') {
      const end = text.indexOf('`', i + 1);
      if (end !== -1) {
        out += `<code>${esc(text.slice(i + 1, end))}</code>`;
        i = end + 1;
        continue;
      }
    }

    // Bold: **...** or __...__
    if (text.startsWith('**', i) || text.startsWith('__', i)) {
      const mark = text.slice(i, i + 2);
      const end = text.indexOf(mark, i + 2);
      if (end !== -1 && end > i + 2) {
        out += `<strong>${inline(text.slice(i + 2, end))}</strong>`;
        i = end + 2;
        continue;
      }
    }

    // Italic: *...* (not part of a ** marker)
    if (ch === '*' && text[i + 1] !== '*') {
      const end = text.indexOf('*', i + 1);
      if (end !== -1 && end > i + 1) {
        out += `<em>${inline(text.slice(i + 1, end))}</em>`;
        i = end + 1;
        continue;
      }
    }

    // Links: [label](https://...)
    if (ch === '[') {
      const close = text.indexOf(']', i + 1);
      if (close !== -1 && text[close + 1] === '(') {
        const end = text.indexOf(')', close + 2);
        if (end !== -1) {
          const label = text.slice(i + 1, close);
          const url = text.slice(close + 2, end).trim();
          if (SAFE_LINK.test(url)) {
            out += `<a href="${esc(url)}" target="_blank" rel="noopener noreferrer">${inline(label)}</a>`;
            i = end + 1;
            continue;
          }
        }
      }
    }

    out += esc(ch);
    i++;
  }

  return out;
}

function renderParagraph(lines) {
  return `<p>${lines.map(inline).join('<br>')}</p>`;
}

// A bullet like "fe (seq 654): tick 暂停" gets its label bolded. This is what
// makes long incident reports scannable: the reader can jump between agents
// without reading every sentence.
function renderItem(text) {
  const m = text.match(/^([^：:]{1,40})([：:])\s*(.*)$/);
  if (m) {
    const label = m[1].trim();
    if (
      label &&
      !/^https?$/i.test(label) &&
      !/[/\\]/.test(label) &&
      /[A-Za-z\u3400-\u9FFF]/.test(label) &&
      !/[。！？!?]/.test(label)
    ) {
      return `<strong class="msg-li-label">${inline(label)}${m[2]}</strong>${m[3] ? ' ' + inline(m[3]) : ''}`;
    }
  }
  return inline(text);
}

function isFence(line) {
  return /^\s*(`{3,}|~{3,})\s*[\w.+-]*\s*$/.test(line);
}

// Split a pipe-table row. A leading/trailing pipe is optional; "\|" is a
// literal pipe inside a cell. Kept deliberately small: agents write compact
// status tables, not arbitrary CSV with quoted separators.
function splitTableRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);

  const cells = [];
  let cur = '';
  let inCode = false;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    // A backslash only escapes a pipe in a table cell; leave other backslashes
    // (Windows paths, regexes) alone.
    if (ch === '\\' && s[i + 1] === '|') {
      cur += '|';
      i++;
    } else if (ch === '`') {
      inCode = !inCode;
      cur += ch;
    } else if (ch === '|' && !inCode) {
      cells.push(cur.trim());
      cur = '';
    } else {
      cur += ch;
    }
  }
  cells.push(cur.trim());
  return cells;
}

function isTableSeparator(line) {
  const cells = splitTableRow(line);
  return cells.length > 0 && cells.every((cell) => /^:?-{3,}:?$/.test(cell));
}

function isTableStart(lines, i) {
  return i + 1 < lines.length && lines[i].includes('|') && isTableSeparator(lines[i + 1]);
}

function tableAlignClass(separatorCell) {
  const left = separatorCell.startsWith(':');
  const right = separatorCell.endsWith(':');
  if (left && right) return ' class="msg-align-center"';
  if (right) return ' class="msg-align-right"';
  return '';
}

function renderTable(lines, start) {
  const header = splitTableRow(lines[start]);
  const separators = splitTableRow(lines[start + 1]);
  const alignments = header.map((_, idx) => separators[idx] || '---');
  let i = start + 2;
  const rows = [];

  while (i < lines.length && lines[i].trim() && lines[i].includes('|')) {
    let cells = splitTableRow(lines[i]);
    // Missing cells become empty; extra cells are dropped so the header row
    // controls the column count and a ragged table still renders predictably.
    while (cells.length < header.length) cells.push('');
    if (cells.length > header.length) cells = cells.slice(0, header.length);
    rows.push(cells);
    i++;
  }

  const th = header.map((cell, idx) => `<th${tableAlignClass(alignments[idx])}>${inline(cell)}</th>`).join('');
  const body = rows.map((row) => '<tr>' + row.map((cell, idx) =>
    `<td${tableAlignClass(alignments[idx])}>${inline(cell)}</td>`
  ).join('') + '</tr>').join('');

  return {
    html: `<div class="msg-table-wrap"><table class="msg-table"><thead><tr>${th}</tr></thead><tbody>${body}</tbody></table></div>`,
    next: i,
  };
}

function isBlockStart(line) {
  const s = line.trim();
  return (
    !s ||
    isFence(line) ||
    /^#{1,4}\s+/.test(s) ||
    /^[一二三四五六七八九十百]+、/.test(s) ||
    /^[-*_]{3,}\s*$/.test(s) ||
    /^>\s?/.test(s) ||
    /^\s*[-*•]\s+/.test(line) ||
    /^\s*\d+[.)、]\s+/.test(line) ||
    /^(?:共同)?(?:结论|小结|总结)[：:]/.test(s)
  );
}

function renderList(lines, start, ordered) {
  const tag = ordered ? 'ol' : 'ul';
  const itemRe = ordered ? /^(\s*)\d+[.)、]\s+(.+)$/ : /^(\s*)[-*•]\s+(.+)$/;
  const items = [];
  let i = start;

  while (i < lines.length) {
    const m = lines[i].match(itemRe);
    if (!m) break;

    let content = m[2];
    i++;

    // Fold indented continuation lines into the same item. A nested list would
    // need a richer model; reports overwhelmingly use one flat level.
    while (
      i < lines.length &&
      lines[i].trim() &&
      /^\s+/.test(lines[i]) &&
      !itemRe.test(lines[i]) &&
      !isBlockStart(lines[i]) &&
      !isTableStart(lines, i)
    ) {
      content += ' ' + lines[i].trim();
      i++;
    }

    items.push(`<li>${renderItem(content)}</li>`);
  }

  return { html: `<${tag}>${items.join('')}</${tag}>`, next: i };
}

export function renderMessage(value) {
  const text = String(value ?? '').replace(/\r\n?/g, '\n');
  if (!text.trim()) return '';

  const lines = text.split('\n');
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Blank lines only separate blocks; they do not need output of their own.
    if (!line.trim()) {
      i++;
      continue;
    }

    // Fenced code block.
    if (isFence(line)) {
      const fenceChar = line.trim()[0];
      const code = [];
      i++;
      while (i < lines.length) {
        const candidate = lines[i].trim();
        if (candidate.length >= 3 && candidate[0] === fenceChar && /^(`{3,}|~{3,})$/.test(candidate)) {
          i++;
          break;
        }
        code.push(lines[i]);
        i++;
      }
      out.push(`<pre class="msg-code"><code>${esc(code.join('\n'))}</code></pre>`);
      continue;
    }

    // Markdown heading. Keep message headings compact so they do not compete
    // with the page's own hierarchy.
    const heading = line.match(/^(#{1,4})\s+(.+)$/);
    if (heading) {
      const level = heading[1].length <= 2 ? 3 : 4;
      out.push(`<h${level} class="msg-heading">${inline(heading[2])}</h${level}>`);
      i++;
      continue;
    }

    // Chinese report section heading: "一、hook 同病检查回执...".
    if (/^[一二三四五六七八九十百]+、/.test(line.trim())) {
      out.push(`<h4 class="msg-heading msg-section">${inline(line.trim())}</h4>`);
      i++;
      continue;
    }

    // Horizontal rule.
    if (/^[-*_]{3,}\s*$/.test(line.trim())) {
      out.push('<hr>');
      i++;
      continue;
    }

    // Blockquote.
    if (/^>\s?/.test(line.trim())) {
      const quote = [];
      while (i < lines.length && /^>\s?/.test(lines[i].trim())) {
        quote.push(lines[i].trim().replace(/^>\s?/, ''));
        i++;
      }
      out.push(`<blockquote>${renderParagraph(quote)}</blockquote>`);
      continue;
    }

    // Lists.
    if (/^\s*[-*•]\s+/.test(line)) {
      const list = renderList(lines, i, false);
      out.push(list.html);
      i = list.next;
      continue;
    }
    if (/^\s*\d+[.)、]\s+/.test(line)) {
      const list = renderList(lines, i, true);
      out.push(list.html);
      i = list.next;
      continue;
    }

    // Pipe tables: header row + | --- | separator, then body rows.
    if (isTableStart(lines, i)) {
      const table = renderTable(lines, i);
      out.push(table.html);
      i = table.next;
      continue;
    }

    // A closing "共同结论：" / "结论：" callout stands out from the body.
    const summary = line.trim().match(/^((?:共同)?(?:结论|小结|总结))([：:])\s*(.*)$/);
    if (summary) {
      let rest = inline(summary[3]);
      i++;
      while (i < lines.length && lines[i].trim() && !isBlockStart(lines[i])) {
        rest += (rest ? '<br>' : '') + inline(lines[i].trim());
        i++;
      }
      out.push(
        `<div class="msg-summary"><span class="msg-summary-k">${inline(summary[1])}${summary[2]}</span>${rest ? ' ' + rest : ''}</div>`
      );
      continue;
    }

    // Paragraph: consecutive plain lines keep their explicit line breaks.
    const para = [];
    while (i < lines.length && lines[i].trim() && !isBlockStart(lines[i]) && !isTableStart(lines, i)) {
      para.push(lines[i]);
      i++;
    }
    if (para.length === 0) {
      // Defensive fallback: an unclassified line must not spin forever.
      para.push(line);
      i++;
    }
    out.push(renderParagraph(para));
  }

  return out.join('');
}

// Long messages get a collapsed first view with an "展开全文" control. The
// thresholds are deliberately conservative: short chat should never be hidden.
export function messageIsLong(value) {
  const text = String(value ?? '');
  if (text.length >= 1200) return true;
  return text.split('\n').filter((line) => line.trim()).length >= 16;
}
