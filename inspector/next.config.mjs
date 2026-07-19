/** @type {import('next').NextConfig} */
const nextConfig = {
  // Emit a self-contained server bundle so the production image stays small.
  output: "standalone",
  reactCompiler: true,
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
};

export default nextConfig;
