import { create } from 'zustand';
import type { AccountConfig, AccountStats, AccountImportInput, AdminRefreshResponse } from '../../domain/account/entity';
import { LocalAccountRepositoryImpl } from '../../infrastructure/repositories/account.repo.impl';

const repo = new LocalAccountRepositoryImpl();

interface AccountState {
  accounts: AccountStats[];
  totalCount: number;
  readyCount: number;
  credsFile: string;
  loading: boolean;
  /** 最近一次拉取失败的原因（成功后清空） */
  error: string;
  selectedAccount: AccountStats | null;
  detailDrawerOpen: boolean;
  importModalOpen: boolean;
  editingAccount: AccountStats | null;
  editModalOpen: boolean;
  accountNameMap: Map<string, string>; // 账号 ID -> 名称（统计页 hover 用）

  // Actions
  fetchAccounts: () => Promise<void>;
  createAccount: (account: Partial<AccountStats> | Partial<AccountStats>[]) => Promise<void>;
  updateAccount: (id: string, account: Partial<AccountStats>) => Promise<void>;
  deleteAccount: (id: string) => Promise<void>;
  refreshAccount: (id: string) => Promise<AdminRefreshResponse>;
  reloadPool: () => Promise<void>;
  importAccounts: (input: AccountImportInput) => Promise<void>;
  openDetailDrawer: (account: AccountStats) => void;
  closeDetailDrawer: () => void;
  openEditModal: (account: AccountStats) => void;
  closeEditModal: () => void;
  setImportModalOpen: (open: boolean) => void;
}

export const useAccountStore = create<AccountState>((set, get) => ({
  accounts: [],
  accountNameMap: new Map(),
  totalCount: 0,
  readyCount: 0,
  credsFile: '',
  loading: false,
  error: '',
  selectedAccount: null,
  detailDrawerOpen: false,
  importModalOpen: false,
  editingAccount: null,
  editModalOpen: false,

  fetchAccounts: async () => {
    set({ loading: true });
    try {
      const data = await repo.fetchAccounts();
      const nameMap = new Map<string, string>();
      for (const a of data.accounts) nameMap.set(a.id, a.name);
      set({
        accounts: data.accounts,
        accountNameMap: nameMap,
        totalCount: data.count,
        readyCount: data.ready,
        credsFile: data.creds_file,
        error: '',
      });
    } catch (err: any) {
      // 不向上抛：页面以 error 渲染空态；鉴权失败另由 auth store 引导
      set({ error: err?.message || '加载失败' });
    } finally {
      set({ loading: false });
    }
  },

  createAccount: async (account) => {
    set({ loading: true });
    try {
      await repo.createAccount(account);
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  updateAccount: async (id, partial) => {
    set({ loading: true });
    try {
      await repo.updateAccount(id, partial);
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  deleteAccount: async (id: string) => {
    set({ loading: true });
    try {
      await repo.deleteAccount(id);
      await get().fetchAccounts();
      if (get().selectedAccount?.id === id) {
        set({ selectedAccount: null, detailDrawerOpen: false });
      }
    } finally {
      set({ loading: false });
    }
  },

  refreshAccount: async (id: string) => {
    const res = await repo.refreshAccount(id);
    await get().fetchAccounts();
    return res;
  },

  reloadPool: async () => {
    set({ loading: true });
    try {
      await repo.reloadPool();
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  importAccounts: async (input: AccountImportInput) => {
    let batch: AccountConfig[];
    if (input.accounts) {
      batch = input.accounts;
    } else {
      const text = input.rawText.trim();
      if (!text) throw new Error('导入内容不能为空');

      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch {
        if (/^[{["]/.test(text)) {
          throw new Error('JSON 格式无效，请修正后重新导入');
        }
      }

      if (parsed !== undefined) {
        const wrapped = parsed && typeof parsed === 'object' && !Array.isArray(parsed) && 'accounts' in parsed;
        const list = wrapped ? (parsed as { accounts: unknown }).accounts : Array.isArray(parsed) ? parsed : [parsed];
        if (!Array.isArray(list)) throw new Error('accounts 必须是账号数组');
        batch = list.map((value: unknown) => {
          if (!value || typeof value !== 'object' || Array.isArray(value)) {
            throw new Error('每个账号必须是 JSON 对象');
          }
          const item = value as Record<string, unknown>;
          return {
            ...item,
            name: item.name ?? item.id ?? '新建账号',
            cookies: item.cookies ?? item.cookie,
            session_token: item.session_token ?? item.sessionToken,
            access_token: item.access_token ?? item.accessToken,
            refresh_token: item.refresh_token ?? item.refreshToken,
            max_concurrency: item.max_concurrency ?? item.maxConcurrency ?? 2,
          } as AccountConfig;
        });
      } else {
        const lines = text.split('\n').map((line) => line.trim()).filter(Boolean);
        if (lines.some((line) => !line.includes('='))) {
          throw new Error('Cookie 格式无效，请输入完整的 name=value Cookie 或有效 JSON');
        }
        batch = lines.map((cookies, index) => ({
          name: input.name || `Cookie账号 #${index + 1}`,
          cookies,
          max_concurrency: 2,
        }));
      }
    }

    if (batch.length === 0) {
      throw new Error('未能识别出有效的账号凭据内容');
    }

    set({ loading: true });
    try {
      await repo.importAccounts(batch, input.verify ?? true);
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  openDetailDrawer: (account: AccountStats) => {
    set({ selectedAccount: account, detailDrawerOpen: true });
  },

  closeDetailDrawer: () => {
    set({ detailDrawerOpen: false, selectedAccount: null });
  },

  openEditModal: (account: AccountStats) => {
    set({ editingAccount: account, editModalOpen: true });
  },

  closeEditModal: () => {
    set({ editModalOpen: false, editingAccount: null });
  },

  setImportModalOpen: (open: boolean) => {
    set({ importModalOpen: open });
  },
}));
