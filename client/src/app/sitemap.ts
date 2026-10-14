import type { MetadataRoute } from "next";
import { SITE_URL } from "@/lib/site";
import { LISTED } from "@/lib/tokens";

export default function sitemap(): MetadataRoute.Sitemap {
  const page = (path: string, priority: number) => ({ url: `${SITE_URL}${path}`, changeFrequency: "daily" as const, priority });
  return [page("/", 1), page("/baskets", 0.9), page("/baskets/new", 0.6), page("/risk", 0.5), ...LISTED.map((m) => page(`/trade/${m.symbol}`, 0.8))];
}
