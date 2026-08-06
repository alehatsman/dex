import { capitalize } from "@bright/common";
import { fmt } from "@/util";
import React from "react";
import { sib } from "./sibling";

export function run(): string {
  return capitalize(fmt(sib())) + React.version;
}
