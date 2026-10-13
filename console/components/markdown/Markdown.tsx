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
          // Code fences: highlight.js output; hljs escapes its output so
          // dangerouslySetInnerHTML is safe here.
          code({ className: cls, children: codeChildren, ...rest }) {
            const isBlock = !!(rest as { node?: { type?: string } }).node;
            const lang = /language-(\w+)/.exec(cls ?? "")?.[1] ?? null;
            const src =
              typeof codeChildren === "string"
                ? codeChildren
                : Array.isArray(codeChildren)
                  ? codeChildren.join("")
                  : String(codeChildren ?? "");

            if (!isBlock && !cls) {
              // Inline code — no highlighting.
              return <code className={cls}>{codeChildren}</code>;
            }

            const html = highlightCode(src.replace(/\n$/, ""), lang);
            return (
              <pre className={`ksq-markdown__pre${lang ? ` language-${lang}` : ""}`}>
                <code
                  className={cls}
                  dangerouslySetInnerHTML={{ __html: html }}
                />
              </pre>
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
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}
