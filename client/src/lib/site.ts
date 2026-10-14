import type { Metadata } from "next";

// public origin for metadata, robots and sitemap; override per environment (preview deploys)
export const SITE_URL = process.env.NEXT_PUBLIC_SITE_URL ?? "https://terminal.sweem.org";

export const TITLE = "Sweem: The Self Custodial Yield Basket";
export const DESCRIPTION =
  "Build and buy yield baskets across lending, staking, liquidity and funding basis, all from your own wallet. Trade Kuru markets and automate DCA, take profit and stop loss on Monad mainnet.";

// link preview card; a page setting its own openGraph replaces the parent's, so spread this
export const OPEN_GRAPH: NonNullable<Metadata["openGraph"]> = {
  type: "website",
  siteName: "Sweem",
  title: TITLE,
  description: DESCRIPTION,
  locale: "en_US",
  images: [{ url: "/metadata.jpg", width: 3840, height: 2160, alt: TITLE, type: "image/jpeg" }],
};
