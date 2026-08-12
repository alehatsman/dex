import { capitalize } from "@acme/common";
import { fmt } from "@/util";
import React from "react";
import { sib } from "./sibling";
import { helper } from "@acme/common/helper";
import { shim } from "@acme/common/Shim";

export function run(): string {
  return capitalize(fmt(sib())) + React.version + helper() + shim();
}
