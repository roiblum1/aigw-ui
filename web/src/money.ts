// Amounts as the interface shows and takes them. A model with prices is
// counted in credits and shown in dollars; any other is counted in tokens.
// Nothing here needs a browser, so it is tested with node alone.

export type Unit = "tokens" | "credits";

export function formatTokens(n: number): string {
  return `${n.toLocaleString("en-US")} ${n === 1 ? "token" : "tokens"}`;
}

/** A model with prices is counted in credits of this size. */
export const CREDITS_PER_DOLLAR = 100_000;

/** Credits as money: two decimals, and more only for an amount below a cent. */
export function formatDollars(credits: number): string {
  const dollars = credits / CREDITS_PER_DOLLAR;
  const small = dollars !== 0 && Math.abs(dollars) < 0.01;
  return dollars.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: small ? 5 : 2,
  });
}

/** An amount in the unit its model is counted in. */
export function formatAmount(n: number, unit: Unit): string {
  return unit === "credits" ? formatDollars(n) : formatTokens(n);
}

/** The same without the word "tokens", for places that name the unit once. */
export function formatNumber(n: number, unit: Unit): string {
  return unit === "credits" ? formatDollars(n) : n.toLocaleString("en-US");
}

/** What a limit field holds for an amount: dollars for credits, else tokens. */
export function limitText(n: number, unit: Unit): string {
  return unit === "credits" ? String(n / CREDITS_PER_DOLLAR) : String(n);
}

/** The amount a limit field stands for, at least 1 of its unit. */
export function limitValue(text: string, unit: Unit): number {
  const n = Number(text);
  return unit === "credits" ? Math.max(1, Math.round(n * CREDITS_PER_DOLLAR)) : n;
}
