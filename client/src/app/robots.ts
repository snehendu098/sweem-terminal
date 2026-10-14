import type { MetadataRoute } from "next";
import { SITE_URL } from "@/lib/site";

export default function robots(): MetadataRoute.Robots {
  return {
    // portfolio is per user, nothing to index
    rules: { userAgent: "*", allow: "/", disallow: "/portfolio" },
    sitemap: `${SITE_URL}/sitemap.xml`,
  };
}
