import packageJson from "../../package.json";

export const APP_CONFIG = {
  name: "Lore Inspector",
  version: packageJson.version,
  githubUrl: "https://github.com/lore-gpt/lore",
  meta: {
    title: "Lore Inspector",
    description:
      "A read-only self-host diagnostic UI for browsing memories, versions, and run traces on a Lore server.",
  },
};
