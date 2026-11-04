// Mirrors server.BuildInfo (GET /api/v1/version).
export interface VersionInfo {
  version: string;
  commit: string;
  build_timestamp: string;
  builder: string;
  project_url: string;
}
