/**
 * 账号与计划领域实体与值对象
 * 严格与后端 internal/account/account.go 的 Stats 结构体 1:1 对应，杜绝随意编造
 */

export interface AccountStats {
  id: string;
  name: string;
  enabled: boolean;
  available: boolean;
  plan: string;
  email: string;
  has_access_token: boolean;
  has_refresh_token: boolean;
  has_session: boolean;
  token_expires?: string;
  expires_in_sec?: number;
  inflight: number;
  max_concurrency: number;
  cooldown_sec: number;
  fail_streak: number;
  total_requests: number;
  failures: number;
  last_used?: string;
  source: string;
  tags?: string[];
}

export interface AdminAccountsResponse {
  count: number;
  ready: number;
  creds_file: string;
  accounts: AccountStats[];
}

export interface AccountConfig {
  id?: string;
  name?: string;
  enabled?: boolean;
  cookies?: string;
  cookie_map?: Record<string, string>;
  session_token?: string;
  access_token?: string;
  refresh_token?: string;
  expires_at?: string | null;
  account_id?: string;
  email?: string;
  plan?: string;
  proxy?: string;
  max_concurrency?: number;
  rate_per_second?: number;
  rate_burst?: number;
  weight?: number;
  headers?: Record<string, string>;
  tags?: string[];
}

export type AccountImportInput = {
  verify?: boolean;
} & ({ name?: string; rawText: string; accounts?: never } | { accounts: AccountConfig[]; rawText?: never });

export interface AdminAccountImportResponse {
  status: string;
  created: number;
  total: number;
  accounts: { id: string; name: string; email: string; plan: string }[];
}

export interface AdminRefreshResponse {
  account: string;
  source: string;
  plan: string;
  email: string;
  expires_at: string;
  user_id?: string;
  error?: string;
}
