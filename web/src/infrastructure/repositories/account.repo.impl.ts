import type {
  AccountConfig,
  AccountStats,
  AdminAccountImportResponse,
  AdminAccountsResponse,
  AdminRefreshResponse,
} from '../../domain/account/entity';
import type { IAccountRepository } from '../../domain/account/repository';
import { httpClient } from '../http/client';

export class LocalAccountRepositoryImpl implements IAccountRepository {
  /**
   * 直连后端真实 GET /admin/accounts 接口
   */
  async fetchAccounts(): Promise<AdminAccountsResponse> {
    const res = await httpClient.get<AdminAccountsResponse>('/admin/accounts');
    if (!res.data || !Array.isArray(res.data.accounts)) {
      throw new Error('账号列表响应格式无效');
    }
    return res.data;
  }

  /**
   * 直连后端真实 POST /admin/accounts 接口（持久化保存至 SQLite 并热生效）
   */
  async createAccount(account: Partial<AccountStats> | Partial<AccountStats>[]): Promise<void> {
    await httpClient.post('/admin/accounts', account);
  }

  async importAccounts(accounts: AccountConfig[], verify: boolean): Promise<AdminAccountImportResponse> {
    const res = await httpClient.post<AdminAccountImportResponse>('/admin/accounts/import', {
      accounts,
      verify,
    }, { timeout: 180000 });
    return res.data;
  }

  /**
   * 直连后端真实 PUT /admin/accounts/{id} 接口（更新账号属性并持久化至 SQLite）
   */
  async updateAccount(id: string, account: Partial<AccountStats>): Promise<void> {
    await httpClient.put(`/admin/accounts/${encodeURIComponent(id)}`, account);
  }

  /**
   * 直连后端真实 DELETE /admin/accounts/{id} 接口（真正从 SQLite 物理删除并即时从池中剔除）
   */
  async deleteAccount(id: string): Promise<void> {
    await httpClient.delete(`/admin/accounts/${encodeURIComponent(id)}`);
  }

  /**
   * 直连后端真实 POST /admin/accounts/{id}/refresh 接口
   */
  async refreshAccount(id: string): Promise<AdminRefreshResponse> {
    const res = await httpClient.post<AdminRefreshResponse>(`/admin/accounts/${encodeURIComponent(id)}/refresh`);
    return res.data;
  }

  /**
   * 直连后端真实 POST /admin/reload 接口
   */
  async reloadPool(): Promise<{ accounts: number }> {
    const res = await httpClient.post<{ accounts: number }>('/admin/reload');
    return res.data;
  }
}
