import type { AccountStats } from './entity';

/**
 * 账号领域辅助服务
 */
export class AccountDomainService {
  /**
   * 检查账号是否属于健康活跃态
   */
  static isHealthy(account: AccountStats): boolean {
    return account.available;
  }
}
