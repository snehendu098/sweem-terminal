import type { Metadata } from "next";
import { Suspense } from "react";
import { OPEN_GRAPH } from "@/lib/site";
import { HomePage } from "@/components/home/HomePage";

// only the home page is canonical to the root; other routes keep their own URL
export const metadata: Metadata = { alternates: { canonical: "/" }, openGraph: { ...OPEN_GRAPH, url: "/" } };

export default function Home() {
  return (
    <Suspense>
      <HomePage />
    </Suspense>
  );
}
