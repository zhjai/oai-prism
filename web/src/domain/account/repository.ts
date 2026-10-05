import type { AccountConfig, AccountStats, AdminAccountImportResponse, AdminAccountsResponse, AdminRefreshResponse } from './entity';

/**
 * 账号领域仓储接口（支持完整 CRUD 与持久化）
 */
export interface IAccountRepository {
  fetchAccounts(): Promise<AdminAccountsResponse>;
  createAccount(account: Partial<AccountStats> | Partial<AccountStats>[]): Promise<void>;
  importAccounts(accounts: AccountConfig[], verify: boolean): Promise<AdminAccountImportResponse>;
  updateAccount(id: string, account: Partial<AccountStats>): Promise<void>;
  deleteAccount(id: string): Promise<void>;
  refreshAccount(id: string): Promise<AdminRefreshResponse>;
  reloadPool(): Promise<{ accounts: number }>;
}
