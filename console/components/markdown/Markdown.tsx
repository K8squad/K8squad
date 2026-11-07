import hljs from "highlight.js/lib/core";
import hlBash from "highlight.js/lib/languages/bash";
import hlCss from "highlight.js/lib/languages/css";
import hlDockerfile from "highlight.js/lib/languages/dockerfile";
import hlGo from "highlight.js/lib/languages/go";
import hlJavascript from "highlight.js/lib/languages/javascript";
import hlJson from "highlight.js/lib/languages/json";
import hlMarkdown from "highlight.js/lib/languages/markdown";
import hlPython from "highlight.js/lib/languages/python";
import hlRust from "highlight.js/lib/languages/rust";
import hlShell from "highlight.js/lib/languages/shell";
import hlSql from "highlight.js/lib/languages/sql";
import hlTypescript from "highlight.js/lib/languages/typescript";
import hlXml from "highlight.js/lib/languages/xml";
import hlYaml from "highlight.js/lib/languages/yaml";
import React, { useMemo } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

import "./markdown.css";

hljs.registerLanguage("bash", hlBash);
hljs.registerLanguage("css", hlCss);
hljs.registerLanguage("dockerfile", hlDockerfile);
hljs.registerLanguage("go", hlGo);
hljs.registerLanguage("javascript", hlJavascript);
hljs.registerLanguage("js", hlJavascript);
hljs.registerLanguage("json", hlJson);
hljs.registerLanguage("markdown", hlMarkdown);
hljs.registerLanguage("md", hlMarkdown);
hljs.registerLanguage("python", hlPython);
hljs.registerLanguage("py", hlPython);
hljs.registerLanguage("rust", hlRust);
hljs.registerLanguage("sh", hlShell);
hljs.registerLanguage("shell", hlShell);
hljs.registerLanguage("sql", hlSql);
hljs.registerLanguage("ts", hlTypescript);
hljs.registerLanguage("tsx", hlTypescript);
hljs.registerLanguage("typescript", hlTypescript);
hljs.registerLanguage("xml", hlXml);
hljs.registerLanguage("yaml", hlYaml);
hljs.registerLanguage("yml", hlYaml);

/** ~1 MiB — matches readserver maxReadBytes; above this we skip the parser. */
const MARKDOWN_MAX_BYTES = 1 * 1024 * 1024;

interface Props {
  children: string;
  className?: string;
  /** data-testid forwarded to the root element */
  testId?: string;
}

/**
 * Shared hardened Markdown renderer.
 *
 * - NO rehype-raw: raw HTML in agent/user content is never evaluated.
 * - External links open in a new tab with noopener noreferrer.
 * - Oversize (>1 MiB) or parse-error input falls back to pre-wrap plain text.
 * - Code fences are syntax-highlighted via highlight.js (output is escaped by hljs).
 */
export function Markdown({ children, className, testId }: Props) {
  const tooLarge = useMemo(
    () => new TextEncoder().encode(children).length > MARKDOWN_MAX_BYTES,
    [children],
  );

  if (tooLarge) {
    return (
      <pre
        className={`ksq-markdown ksq-markdown--plain${className ? ` ${className}` : ""}`}
        data-testid={testId}
      >
        {children}
      </pre>
    );
  }

  return (
    <div
      className={`ksq-markdown${className ? ` ${className}` : ""}`}
      data-testid={testId}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          // External links: noopener + new tab; internal/relative links unchanged.
          a({ href, children: linkChildren, ...rest }) {
            const isExternal =
              typeof href === "string" &&
              (href.startsWith("http://") || href.startsWith("https://"));
            return (
              <a
                href={href}
                {...(isExternal
                  ? { target: "_blank", rel: "noopener noreferrer" }
                  : {})}
                {...rest}
              >
                {linkChildren}
              </a>
            );
          },
          // Block fence container: exactly ONE <pre> per code block (ISI-7197).
          // The code component below must NOT emit its own <pre> — doing so
          // rendered fences as pre-in-pre and inline code as a <pre> inside
          // <p> (the renderer-side artifacts WS-5 was scoped to find).
          pre({ children }) {
            return <pre className="ksq-markdown__pre">{children}</pre>;
          },
          // Code elements: highlight ONLY fenced code that names a language
          // (block code carries a language-* className). Inline spans — and
          // fences with no language tag — render as plain <code>:
          // react-markdown v9+ passes `node` for inline code too, so node
          // presence is NOT a block discriminator.
          code({ className: cls, children: codeChildren }) {
            const lang = /language-(\w+)/.exec(cls ?? "")?.[1] ?? null;
            if (!lang) {
              // Inline code — no highlighting.
              return <code className={cls}>{codeChildren}</code>;
            }
            const src =
              typeof codeChildren === "string"
                ? codeChildren
                : Array.isArray(codeChildren)
                  ? codeChildren.join("")
                  : String(codeChildren ?? "");

            const html = highlightCode(src.replace(/\n$/, ""), lang);
            return (
              <code
                className={cls}
                dangerouslySetInnerHTML={{ __html: html }}
              />
            );
          },
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}

function highlightCode(source: string, language: string | null): string {
  if (language && hljs.getLanguage(language)) {
    try {
      return hljs.highlight(source, { language }).value;
    } catch {
      // fall through
    }
  }
  return escapeHtml(source);
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
