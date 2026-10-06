import { create } from 'zustand';
import type { ChatAttachment, ChatMessage, ChatModelInfo, ChatSession, ChatUsage, ReasoningEffort } from '../../domain/chat/entity';
import { effortsForModel } from '../../domain/modelFilter';
import { ChatRepositoryImpl } from '../../infrastructure/repositories/chat.repo.impl';

const repo = new ChatRepositoryImpl();
let catalogRequest = 0;

const newId = (prefix: string) => `${prefix}_${Array.from(crypto.getRandomValues(new Uint32Array(4)), (part) => part.toString(16).padStart(8, '0')).join('')}`;

interface ChatState {
  models: ChatModelInfo[];
  allModelIds: string[]; // 全量 id（含档位变体）—— 推导各模型的可用推理档位
  sessions: ChatSession[];
  currentSessionId: string | null;
  selectedModel: string;
  reasoningEffort: ReasoningEffort;
  selectedAccountId: string;
  isStreaming: boolean;
  lastUsage: ChatUsage | null; // 本轮 token 用量（上下文窗口可视化）
  catalogError: string;
  isCatalogLoading: boolean;

  // Actions
  init: () => Promise<void>;
  refreshModels: () => Promise<void>;
  selectSession: (id: string) => void;
  createNewSession: () => void;
  deleteSession: (id: string) => Promise<void>;
  renameSession: (id: string, title: string) => void;
  setModel: (model: string) => void;
  setReasoningEffort: (effort: ReasoningEffort) => void;
  setAccount: (id: string) => void;
  sendMessage: (text: string, attachments?: ChatAttachment[]) => Promise<void>;
}

export const useChatStore = create<ChatState>((set, get) => ({
  models: [],
  allModelIds: [],
  sessions: [],
  currentSessionId: null,
  selectedModel: '',
  reasoningEffort: 'medium',
  selectedAccountId: '',
  isStreaming: false,
  lastUsage: null,
  catalogError: '',
  isCatalogLoading: false,

  init: async () => {
    const sessions = await repo.listSessions();
    const defaultSessionId = sessions[0]?.id || null;
    set({
      sessions,
      currentSessionId: defaultSessionId,
      selectedAccountId: sessions[0]?.accountId || '',
      selectedModel: sessions[0]?.model || '',
      reasoningEffort: sessions[0]?.reasoningEffort || 'medium',
    });
    await get().refreshModels();
    if (!get().sessions.length && get().selectedModel) get().createNewSession();
  },

  refreshModels: async () => {
    if (get().isStreaming) return;
    const request = ++catalogRequest;
    const accountId = get().selectedAccountId;
    set({ isCatalogLoading: true, catalogError: '' });
    try {
      const { mains, allIds } = await repo.fetchModelCatalog(accountId);
      if (request !== catalogRequest) return;
      const previous = get().selectedModel;
      const selectedModel = previous || mains[0]?.id || '';
      const available = effortsForModel(selectedModel, allIds, mains.find((m) => m.id === selectedModel)?.reasoningEfforts);
      const preferred = mains.find((m) => m.id === selectedModel)?.defaultReasoningEffort;
      set({
        models: mains, allModelIds: allIds, selectedModel,
        reasoningEffort: available.includes(get().reasoningEffort) ? get().reasoningEffort : (preferred && available.includes(preferred) ? preferred : available[0] || 'medium'),
        catalogError: !mains.length ? '当前账号范围没有可用模型，请检查账号状态和 Key 绑定' : (previous && !mains.some((m) => m.id === previous) ? '之前的模型已不可用，请重新选择模型' : ''),
        isCatalogLoading: false,
      });
      if (!get().sessions.length && selectedModel) get().createNewSession();
    } catch (err: any) {
      if (request !== catalogRequest) return;
      set({ models: [], allModelIds: [], isCatalogLoading: false, catalogError: err.message || '模型目录获取失败' });
    }
  },

  selectSession: (id: string) => {
    const session = get().sessions.find((s) => s.id === id);
    set({ currentSessionId: id, selectedAccountId: session?.accountId || '', selectedModel: session?.model || '', reasoningEffort: session?.reasoningEffort || 'medium' });
    void get().refreshModels();
  },

  createNewSession: () => {
    const newSession: ChatSession = {
      id: newId('sess'),
      title: '新调试会话',
      model: get().selectedModel,
      reasoningEffort: get().reasoningEffort,
      accountId: get().selectedAccountId,
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      messages: [],
    };
    const sessions = [newSession, ...get().sessions];
    repo.saveSession(newSession);
    set({ sessions, currentSessionId: newSession.id });
  },

  deleteSession: async (id: string) => {
    await repo.deleteSession(id);
    const sessions = get().sessions.filter((s) => s.id !== id);
    const nextId = sessions[0]?.id || null;
    set({ sessions, currentSessionId: nextId, selectedAccountId: sessions[0]?.accountId || '', selectedModel: sessions[0]?.model || '' });
    void get().refreshModels();
  },

  renameSession: (id: string, title: string) => {
    const trimmed = title.trim();
    if (!trimmed) return;
    const sessions = get().sessions.map((s) =>
      s.id === id ? { ...s, title: trimmed, updatedAt: new Date().toISOString() } : s,
    );
    set({ sessions });
    const updated = sessions.find((s) => s.id === id);
    if (updated) repo.saveSession(updated);
  },

  setModel: (model: string) => {
    // 切换模型时校验当前推理档位：新模型不支持则回落 medium
    // （各模型的档位由后端配置决定，如 6 Luna 没有 low）
    const { allModelIds, reasoningEffort, models } = get();
    const available = effortsForModel(model, allModelIds, models.find((m) => m.id === model)?.reasoningEfforts);
    const preferred = models.find((m) => m.id === model)?.defaultReasoningEffort;
    set({
      selectedModel: model,
      reasoningEffort: available.includes(reasoningEffort) ? reasoningEffort : (preferred && available.includes(preferred) ? preferred : available[0] || 'medium'),
      catalogError: '',
    });
  },

  setReasoningEffort: (effort: ReasoningEffort) => {
    set({ reasoningEffort: effort });
  },

  setAccount: (id: string) => {
    const sessions = get().sessions.map((s) => s.id === get().currentSessionId ? { ...s, accountId: id } : s);
    set({ selectedAccountId: id, sessions });
    const updated = sessions.find((s) => s.id === get().currentSessionId);
    if (updated) void repo.saveSession(updated);
    void get().refreshModels();
  },

  sendMessage: async (text: string, attachments?: ChatAttachment[]) => {
    const { currentSessionId, selectedModel, reasoningEffort, selectedAccountId, sessions } = get();
    if (!text.trim() || !currentSessionId || get().isStreaming || get().isCatalogLoading || !get().models.some((m) => m.id === selectedModel)) return;

    const session = sessions.find((s) => s.id === currentSessionId);
    if (!session) return;

    const userMsg: ChatMessage = {
      id: newId('msg_u'),
      role: 'user',
      content: text,
      attachments: attachments && attachments.length > 0 ? attachments : undefined,
      createdAt: new Date().toISOString(),
      status: 'success',
    };

    const assistantMsgId = newId('msg_a');
    const assistantMsg: ChatMessage = {
      id: assistantMsgId,
      role: 'assistant',
      content: '',
      reasoning: '',
      progress: '',
      createdAt: new Date().toISOString(),
      status: 'loading',
    };

    const updatedMessages = [...session.messages, userMsg, assistantMsg];
    const updatedSession: ChatSession = {
      ...session,
      model: selectedModel,
      reasoningEffort,
      title: session.messages.length === 0 ? text.slice(0, 18) : session.title,
      messages: updatedMessages,
      updatedAt: new Date().toISOString(),
    };

    const updatedSessions = sessions.map((s) => (s.id === currentSessionId ? updatedSession : s));
    set({ sessions: updatedSessions, isStreaming: true, lastUsage: null });

    let currentContent = '';
    let currentReasoning = '';
    let currentProgress = '';

    await repo.sendMessageStream({
      sessionId: currentSessionId,
      userMessageId: userMsg.id,
      assistantMessageId: assistantMsgId,
      content: text,
      attachments,
      model: selectedModel,
      reasoningEffort,
      accountId: selectedAccountId,
      // 上游不代管对话历史：把既有消息（历史轮）回传给网关拼进上下文。
      // 只取到 userMsg 为止，不含 assistantMsg 占位（它此刻还是空的）。
      history: [...session.messages],
      onUsage: (usage) => set({ lastUsage: usage }),
      onChunk: (chunk, reasoningChunk, progressChunk) => {
        if (chunk) currentContent += chunk;
        if (reasoningChunk) currentReasoning += reasoningChunk;
        if (progressChunk) currentProgress += progressChunk;
        if (!chunk && !reasoningChunk && !progressChunk) return;

        const liveSessions = get().sessions.map((s) => {
          if (s.id !== currentSessionId) return s;
          const msgs = s.messages.map((m) => {
            if (m.id !== assistantMsgId) return m;
            return {
              ...m,
              content: currentContent,
              reasoning: currentReasoning,
              progress: currentProgress,
              status: 'loading' as const,
            };
          });
          return { ...s, messages: msgs };
        });
        set({ sessions: liveSessions });
      },
      onFinish: () => {
        const finalSessions = get().sessions.map((s) => {
          if (s.id !== currentSessionId) return s;
          const msgs = s.messages.map((m) => {
            if (m.id !== assistantMsgId) return m;
            return {
              ...m,
              content: currentContent,
              reasoning: currentReasoning,
              progress: currentProgress,
              status: 'success' as const,
            };
          });
          const completedSession = { ...s, messages: msgs };
          repo.saveSession(completedSession);
          return completedSession;
        });
        set({ sessions: finalSessions, isStreaming: false });
      },
      onError: (err) => {
        const errorSessions = get().sessions.map((s) => {
          if (s.id !== currentSessionId) return s;
          const msgs = s.messages.map((m) => {
            if (m.id !== assistantMsgId) return m;
            return {
              ...m,
              content: `${currentContent ? currentContent + '\n\n' : ''}[请求失败] ${err.message}`,
              reasoning: currentReasoning,
              progress: currentProgress,
              status: 'error' as const,
            };
          });
          return { ...s, messages: msgs };
        });
        set({ sessions: errorSessions, isStreaming: false });
      },
    });
  },
}));
