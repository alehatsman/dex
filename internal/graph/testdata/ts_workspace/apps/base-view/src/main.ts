import { capitalize } from "@bright/common";
import { fmt } from "@/util";
import React from "react";
import { sib } from "./sibling";
import { helper } from "@bright/common/helper";
import { shim } from "@bright/common/Shim";

export function run(): string {
  return capitalize(fmt(sib())) + React.version + helper() + shim();
}
