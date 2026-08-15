// CodeMirror 6 封装：创建规则文件编辑器，提供 setText / getText / destroy。
// 规则文件是纯文本（非 JSON），用行级装饰做语法高亮 + 行号。

import { EditorState, type Extension } from "@codemirror/state";
import {
  EditorView,
  Decoration,
  type DecorationSet,
  ViewPlugin,
  type ViewUpdate,
  lineNumbers,
  highlightActiveLine,
  keymap,
} from "@codemirror/view";
import { history, defaultKeymap, historyKeymap } from "@codemirror/commands";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags } from "@lezer/highlight";

// ---------- 主题高亮 ----------

const highlightStyle = HighlightStyle.define([
  { tag: tags.comment, color: "#94a3b8", fontStyle: "italic" },
  { tag: tags.keyword, color: "#f97316", fontWeight: "bold" },
  { tag: tags.string, color: "#10b981" },
]);

const highlightStyleDark = HighlightStyle.define([
  { tag: tags.comment, color: "#64748b", fontStyle: "italic" },
  { tag: tags.keyword, color: "#fb923c", fontWeight: "bold" },
  { tag: tags.string, color: "#34d399" },
]);

// ---------- 行级装饰：注释行 / 行为关键字行 ----------

const rulesHighlighter = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet;
    constructor(view: EditorView) {
      this.decorations = this.build(view);
    }
    update(u: ViewUpdate) {
      if (u.docChanged || u.viewportChanged) {
        this.decorations = this.build(u.view);
      }
    }
    build(view: EditorView): DecorationSet {
      const decos: { from: number; value: ReturnType<typeof Decoration.line> }[] = [];
      for (const { from, to } of view.visibleRanges) {
        for (let pos = from; pos <= to; ) {
          const line = view.state.doc.lineAt(pos);
          const text = line.text.trimStart();
          if (text.startsWith("#")) {
            decos.push({ from: line.from, value: Decoration.line({ class: "cm-comment-line" }) });
          } else if (/^(REJECT|reject)\b/.test(text)) {
            decos.push({ from: line.from, value: Decoration.line({ class: "cm-reject-line" }) });
          } else if (/^(proxy|direct)\b/.test(text)) {
            decos.push({ from: line.from, value: Decoration.line({ class: "cm-action-line" }) });
          }
          pos = line.to + 1;
        }
      }
      return Decoration.set(
        decos.map((d) => d.value.range(d.from)),
        true,
      );
    }
  },
  {
    decorations: (v: { decorations: DecorationSet }) => v.decorations,
  },
);

// ---------- 编辑器主题 ----------

const editorTheme = EditorView.theme({
  "&": {
    fontSize: "12px",
    fontFamily: "ui-monospace, monospace",
    height: "320px",
    maxHeight: "600px",
  },
  ".cm-scroller": {
    overflow: "auto",
    lineHeight: "20px",
  },
  ".cm-gutters": {
    backgroundColor: "transparent",
    borderRight: "1px solid rgb(203 213 225 / 0.3)",
    color: "rgb(148 163 184)",
  },
  ".cm-content": {
    padding: "8px 0",
  },
  ".cm-lineNumbers .cm-gutterElement": {
    padding: "0 8px",
  },
  ".cm-activeLine": {
    backgroundColor: "rgb(249 115 22 / 0.05)",
  },
  "&.dark .cm-gutters": {
    borderRightColor: "rgb(51 65 85 / 0.5)",
    color: "rgb(100 116 139)",
  },
  ".cm-comment-line": {
    fontStyle: "italic",
  },
  ".cm-reject-line": {
    fontWeight: "bold",
  },
  ".cm-action-line": {
    fontWeight: "500",
  },
});

// ---------- 公开 API ----------

export interface CodeMirrorEditor {
  setText(text: string): void;
  getText(): string;
  destroy(): void;
}

export function createCodeMirrorEditor(
  host: HTMLElement,
  initialText: string,
  onChange: (text: string) => void,
): CodeMirrorEditor {
  const isDark = document.documentElement.classList.contains("dark");

  const extensions: Extension[] = [
    lineNumbers(),
    history(),
    highlightActiveLine(),
    keymap.of([...defaultKeymap, ...historyKeymap]),
    rulesHighlighter,
    syntaxHighlighting(isDark ? highlightStyleDark : highlightStyle),
    editorTheme,
    EditorView.updateListener.of((update: ViewUpdate) => {
      if (update.docChanged) {
        onChange(update.state.doc.toString());
      }
    }),
  ];

  const view = new EditorView({
    state: EditorState.create({
      doc: initialText,
      extensions,
    }),
    parent: host,
  });

  return {
    setText(text: string) {
      if (view.state.doc.toString() === text) return;
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: text },
      });
    },
    getText() {
      return view.state.doc.toString();
    },
    destroy() {
      view.destroy();
    },
  };
}
