import { capitalize, b64encode, Arr } from "@bright/common";
import { ghost } from "@bright/common/loop_a";

// Every callee is bound only through the barrel index.ts re-export table:
//   capitalize — star re-export      (=> String.ts)
//   b64encode  — named+rename        (=> Base64.encode)
//   Arr.first  — namespace re-export  (=> Arr.ts)
//   ghost      — nonexistent, through a re-export cycle (must not bind, must end)
export function run(xs: string[]): string {
  ghost();
  return capitalize(b64encode(Arr.first(xs)));
}
