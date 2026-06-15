import { Handler, makeHandler } from "./handler";
import * as text from "./text";
import { noop } from "./utils";

export interface MainConfig {
  greeting: string;
}

export function helper() {
  return 1;
}

export function main() {
  const h = makeHandler("world");
  const msg = text.upper("hi");
  new Handler(msg);
  helper();
  noop();
}

// Regression (#554): a call inside an object-literal getter body has no
// resolvable function node of its own (the getter is not a class method), so
// the call must be attributed to the enclosing function `withCache`, not
// dropped. Mirrors zod's `extend`/`merge` calling `assignProp` inside a
// `get shape()` getter.
export function withCache() {
  return {
    get value() {
      return helper();
    },
  };
}
