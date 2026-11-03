// SPDX-License-Identifier: AGPL-3.0-only

import { act, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { loadMock } from "../mocks";
import { installDesignSystem } from "../runtime/design-system";
import { jsxText, MockLibrary, type Problem, type Props } from "./render";

declare global {
  var IS_REACT_ACT_ENVIRONMENT: boolean | undefined;
}

/** An artboard file with the given markup inside <x-dc> and the given logic. */
function artboard(body: string, script = "", props = "{}"): string {
  return [
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Test board</title></head><body>`,
    `<x-dc><helmet><style>.from-helmet{color:red}</style></helmet>${body}</x-dc>`,
    `<script type="text/x-dc" data-dc-script data-props='${props}'>${script}</script>`,
    `</body></html>`,
  ].join("\n");
}

const logic = (vals: string) =>
  `class Component extends DCLogic { renderVals() { const h = React.createElement; return ${vals}; } }`;

let logged: unknown[][] = [];
const cleanups: (() => void)[] = [];

beforeAll(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
});

beforeEach(() => {
  logged = [];
  vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
    logged.push(args);
  });
});

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

async function render(
  files: Record<string, string>,
  entry: string,
  options: { props?: Props; components?: Record<string, unknown> } = {},
) {
  const problems: Problem[] = [];
  const library = new MockLibrary({
    load: (file) => {
      const source = files[file];
      if (source == null) throw new Error(`there is no ${file}`);
      return source;
    },
    onProblem: (p) => problems.push(p),
    ...(options.components ? { resolveComponent: (path: string) => options.components?.[path] } : {}),
  });
  await library.load(entry);
  const container = document.createElement("div");
  document.body.append(container);
  const root = createRoot(container);
  await act(async () => root.render(library.element(entry, options.props)));
  cleanups.push(() => {
    act(() => root.unmount());
    container.remove();
  });
  return { container, problems: problems.map((p) => p.message), library };
}

describe("templates", () => {
  it("fills holes in text and attributes, and renders the helmet styles", async () => {
    const { container, problems } = await render(
      {
        "A.dc.html": artboard(
          `<p class="{{cls}}" style="color: {{color}};" data-n="{{n}}">Hello {{who.name}}, {{n}} {{missing}}</p>`,
          logic(`{ cls: 'greeting', color: 'red', n: 3, who: { name: 'Mira' } }`),
        ),
      },
      "A.dc.html",
    );
    expect(problems).toEqual([]);
    const p = container.querySelector("p");
    expect(p?.outerHTML).toBe('<p class="greeting" style="color: red;" data-n="3">Hello Mira, 3 </p>');
    expect(container.querySelector("style")).toBeNull();
    expect(document.head.querySelector('style[data-href="mockview:A.dc.html:0"]')?.textContent).toBe(
      ".from-helmet{color:red}",
    );
    expect(logged).toEqual([]);
  });

  it("repeats <sc-for> with $index, nested and inside a table, without React warnings", async () => {
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<table>
  <tbody>
    <sc-for list="{{rows}}" as="r">
      <tr>
        <sc-for list="{{r.cells}}" as="c"><td>{{$index}}:{{c}}</td></sc-for>
      </tr>
    </sc-for>
  </tbody>
</table>`,
          logic(`{ rows: [{ cells: ['a', 'b'] }, { cells: ['c'] }] }`),
        ),
      },
      "A.dc.html",
    );
    expect([...container.querySelectorAll("tr")].map((tr) => tr.textContent)).toEqual(["0:a1:b", "0:c"]);
    expect(logged).toEqual([]);
  });

  it("branches with <sc-if>", async () => {
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<sc-if value="{{yes}}" hint-placeholder-val="{{false}}"><b>shown</b></sc-if><sc-if value="{{no}}"><i>hidden</i></sc-if>`,
          logic(`{ yes: true, no: '' }`),
        ),
      },
      "A.dc.html",
    );
    expect(container.querySelector("b")?.textContent).toBe("shown");
    expect(container.querySelector("i")).toBeNull();
  });

  it("drops the newline after <pre> and renders elements that a text hole holds", async () => {
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<pre>\nline</pre><div>{{el}}</div><div>{{list}}</div>`,
          logic(`{ el: h('em', null, 'x'), list: [h('u', null, 'a'), h('u', null, 'b')] }`),
        ),
      },
      "A.dc.html",
    );
    expect(container.querySelector("pre")?.textContent).toBe("line");
    expect(container.querySelector("em")?.textContent).toBe("x");
    expect(container.querySelectorAll("u")).toHaveLength(2);
    expect(logged).toEqual([]);
  });

  it("selects the option marked selected and leaves inputs editable", async () => {
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<select><option>a</option><option selected>b</option></select><input value="typed"><input type="checkbox" checked>`,
        ),
      },
      "A.dc.html",
    );
    expect(container.querySelector("select")?.value).toBe("b");
    const [text, box] = container.querySelectorAll("input");
    expect(text?.value).toBe("typed");
    expect(box?.checked).toBe(true);
    expect(logged).toEqual([]);
  });

  it("gives keys to element lists built in a script, so React doesn't warn", async () => {
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<div>{{row}}</div>`,
          logic(`{ row: h('p', null, [h('span', null, 'a'), h('span', null, 'b')]) }`),
        ),
      },
      "A.dc.html",
    );
    expect(container.querySelector("p")?.textContent).toBe("ab");
    expect(logged).toEqual([]);
  });

  it("reports a hole that is an expression and one that holds an object", async () => {
    const { problems } = await render(
      { "A.dc.html": artboard(`<p>{{a + b}}</p><p>{{obj}}</p>`, logic(`{ obj: { x: 1 } }`)) },
      "A.dc.html",
    );
    expect(problems).toEqual([
      "{{a + b}} is not a dotted lookup or a literal, so it renders as nothing. Compute it in renderVals().",
      "A text hole holds an object, which can't be shown as text.",
    ]);
  });
});

it("reads component children the way JSX does", () => {
  expect(jsxText("\n  Deploy\n  now  \n")).toBe("Deploy now");
  expect(jsxText("  one line  ")).toBe("  one line  ");
  expect(jsxText("\n   \n")).toBe("");
});

describe("logic", () => {
  it("gives the script props with data-props defaults, state and lifecycle methods", async () => {
    const script = `class Component extends DCLogic {
  state = { n: 1 };
  componentDidMount() { this.setState({ n: 2 }); }
  renderVals() { return { n: this.state.n, theme: this.props.theme, size: this.props.size }; }
}`;
    const props = `{"theme":{"editor":"enum","options":["light","dark"],"default":"light"},"size":{"default":"md"},"$preview":{"width":10,"height":10}}`;
    const { container, library } = await render(
      { "A.dc.html": artboard(`<p data-theme="{{theme}}">{{n}} {{size}}</p>`, script, props) },
      "A.dc.html",
      { props: { theme: "dark" } },
    );
    expect(container.querySelector("p")?.outerHTML).toBe('<p data-theme="dark">2 md</p>');
    expect(library.artboard("A.dc.html")?.preview).toEqual({ width: 10, height: 10 });
    expect(library.artboard("A.dc.html")?.title).toBe("Test board");
  });

  it("reports a script that doesn't define the Component class, and still draws the markup", async () => {
    const { container, problems } = await render(
      { "A.dc.html": artboard(`<p>static</p>`, `const x = 1;`) },
      "A.dc.html",
    );
    expect(container.textContent).toContain("static");
    expect(problems).toEqual([
      "The logic script doesn't run: A.dc.html: the script doesn't define class Component extends DCLogic.",
    ]);
  });
});

describe("x-import", () => {
  it("mounts a component from the global scope with typed props and children", async () => {
    const seen: Props[] = [];
    const Button = (props: Props) => {
      seen.push(props);
      return <button type="button">{props["children"] as ReactNode}</button>;
    };
    const { container } = await render(
      {
        "A.dc.html": artboard(
          `<x-import component-from-global-scope="DS.Button" variant="primary" size="{{16}}" on-click="{{go}}" mono>
Deploy <b>{{what}}</b>
</x-import>`,
          logic(`{ go: () => 'went', what: 'api' }`),
        ),
      },
      "A.dc.html",
      { components: { "DS.Button": Button } },
    );
    expect(container.querySelector("button")?.innerHTML).toBe("Deploy <b>api</b>");
    expect(seen[0]).toMatchObject({ variant: "primary", size: 16, mono: true });
    const onClick = seen[0]?.["onClick"];
    expect(typeof onClick === "function" ? onClick() : onClick).toBe("went");
  });

  it("reports a component that doesn't exist", async () => {
    const { container, problems } = await render(
      { "A.dc.html": artboard(`<x-import component-from-global-scope="DS.Nope"></x-import>`) },
      "A.dc.html",
      { components: {} },
    );
    expect(problems).toEqual(["DS.Nope isn't a component on window, so <x-import> on line 2 renders as nothing."]);
    expect(container.querySelector("[data-mockview-problem]")).not.toBeNull();
  });

  it("keeps a component that throws from blanking the rest of the artboard", async () => {
    const Broken = () => {
      throw new Error("bad props");
    };
    const { container, problems } = await render(
      {
        "A.dc.html": artboard(`<p>before</p><x-import component-from-global-scope="DS.Broken"></x-import><p>after</p>`),
      },
      "A.dc.html",
      { components: { "DS.Broken": Broken } },
    );
    expect(problems).toEqual(["DS.Broken failed to render: bad props"]);
    expect(container.textContent).toContain("before");
    expect(container.textContent).toContain("after");
    expect(container.querySelector("[data-mockview-problem]")?.textContent).toBe("DS.Broken: bad props");
  });
});

describe("dc-import", () => {
  const child = artboard(
    `<nav data-active="{{active}}" data-mode="{{mode}}">{{label}}</nav>`,
    logic(`{ active: this.props.active, mode: this.props.mode, label: this.props.itemLabel }`),
    `{"active":{"default":"overview"},"mode":{"default":"single"}}`,
  );

  it("renders the imported artboard in place with its defaults and the given props", async () => {
    const { container, problems } = await render(
      {
        "Page.dc.html": artboard(
          `<div class="row"><dc-import name="Nav" active="apps" item-label="Apps" hint-size="1px,2px"></dc-import><main></main></div>`,
        ),
        "Nav.dc.html": child,
      },
      "Page.dc.html",
    );
    expect(problems).toEqual([]);
    expect(container.querySelector(".row")?.firstElementChild?.outerHTML).toBe(
      '<nav data-active="apps" data-mode="single">Apps</nav>',
    );
  });

  it("stops an artboard that imports itself", async () => {
    const { container, problems } = await render(
      {
        "A.dc.html": artboard(`<p>a</p><dc-import name="B"></dc-import>`),
        "B.dc.html": artboard(`<p>b</p><dc-import name="A"></dc-import>`),
      },
      "A.dc.html",
    );
    expect(problems).toEqual([
      "B.dc.html imports A.dc.html inside itself (A.dc.html → B.dc.html → A.dc.html). Remove one of the imports.",
    ]);
    expect([...container.querySelectorAll("p")].map((p) => p.textContent)).toEqual(["a", "b"]);
  });

  it("reports an import that doesn't exist", async () => {
    const { problems } = await render({ "A.dc.html": artboard(`<dc-import name="Gone"></dc-import>`) }, "A.dc.html");
    expect(problems).toEqual([
      "Can't import Gone.dc.html: there is no Gone.dc.html",
      "A.dc.html imports Gone.dc.html, which doesn't exist or isn't loaded.",
    ]);
  });
});

describe("the real mocks", () => {
  it("draws Overview-Single with its rail and top bar, from the design system's bundle", async () => {
    installDesignSystem();
    const { container, problems } = await render(
      {
        "Overview-Single.dc.html": await loadMock("Overview-Single.dc.html"),
        "Rail.dc.html": await loadMock("Rail.dc.html"),
        "Topbar.dc.html": await loadMock("Topbar.dc.html"),
      },
      "Overview-Single.dc.html",
      { props: { theme: "dark" } },
    );
    expect(problems).toEqual([]);
    expect(logged).toEqual([]);
    const root = container.querySelector("[data-theme]");
    expect(root?.getAttribute("data-theme")).toBe("dark");
    expect(root?.querySelector(":scope > nav.dw-rail .is-active")?.textContent).toBe("Overview");
    expect(root?.querySelector("main > header.dw-topbar")?.textContent).toContain("hz-1");
    expect(container.querySelectorAll("table")).toHaveLength(2);
    expect(container.querySelector("h1")?.textContent).toBe("Overview");
  });
});
