/** @type {import('next').NextConfig} */
const nextConfig = {
  // Emit a self-contained server bundle so the production image stays small.
  output: "standalone",
  // The Inspector renders no user images, so Next's image optimization is unused. Disabling it drops the
  // native `sharp` dependency, keeping the standalone bundle arch-independent pure JS — so the multi-arch
  // Docker image cross-builds on the native runner (no QEMU) and stays lean.
  images: { unoptimized: true },
  reactCompiler: true,
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
};

export default nextConfig;
