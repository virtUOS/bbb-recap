// Helpers for tests that look into React elements without rendering them.
import { isValidElement, type ReactNode } from "react";

type Props = { children?: ReactNode; [key: string]: unknown };

function children(node: unknown): unknown[] {
  if (Array.isArray(node)) {
    return node;
  }
  if (isValidElement(node)) {
    const kids = (node.props as Props).children;
    return Array.isArray(kids) ? kids : kids === undefined ? [] : [kids];
  }
  return [];
}

// The text a node shows.
export function textOf(node: unknown): string {
  if (typeof node === "string" || typeof node === "number") {
    return String(node);
  }
  return children(node).map(textOf).join("");
}

// Every element of a type, such as "animate", in a node.
export function elementsOf(node: unknown, type: string): Props[] {
  const own = isValidElement(node) && node.type === type ? [node.props as Props] : [];
  return [...own, ...children(node).flatMap((child) => elementsOf(child, type))];
}
