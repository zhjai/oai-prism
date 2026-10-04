import { create } from 'zustand';
import type { ChatAttachment, ChatMessage, ChatModelInfo, ChatSession, ChatUsage, ReasoningEffort } from '../../domain/chat/entity';
import { effortsForModel } from '../../domain/modelFilter';
import { ChatRepositoryImpl } from '../../infrastructure/repositories/chat.repo.impl';

const repo = new ChatRepositoryImpl();

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

  // Actions
  init: () => Promise<void>;
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
  selectedModel: 'gpt-6.1-sol',
  reasoningEffort: 'medium',
  selectedAccountId: '',
  isStreaming: false,
  lastUsage: null,

  init: async () => {
    const [{ mains, allIds }, sessions] = await Promise.all([
      repo.fetchModelCatalog(),
      repo.listSessions(),
    ]);
    const defaultSessionId = sessions[0]?.id || null;
    set({
      models: mains,
      allModelIds: allIds,
      sessions,
      currentSessionId: defaultSessionId,
      selectedAccountId: sessions[0]?.accountId || '',
      selectedModel: mains[0]?.id || 'gpt-6.1-sol',
      // 兜底：默认档位若不在新模型的可用列表里，回落 medium
      reasoningEffort: effortsForModel(mains[0]?.id || 'gpt-6.1-sol', allIds).includes('medium')
        ? 'medium'
        : (effortsForModel(mains[0]?.id || 'gpt-6.1-sol', allIds)[0] ?? 'medium'),
    });
  },

  selectSession: (id: string) => {
    set({ currentSessionId: id, selectedAccountId: get().sessions.find((s) => s.id === id)?.accountId || '' });
  },

  createNewSession: () => {
    const newSession: ChatSession = {
      id: `sess_${Date.now()}`,
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
    set({ sessions, currentSessionId: nextId, selectedAccountId: sessions[0]?.accountId || '' });
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
    const { allModelIds, reasoningEffort } = get();
    const available = effortsForModel(model, allModelIds);
    set({
      selectedModel: model,
      reasoningEffort: available.includes(reasoningEffort) ? reasoningEffort : 'medium',
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
  },

  sendMessage: async (text: string, attachments?: ChatAttachment[]) => {
    const { currentSessionId, selectedModel, reasoningEffort, selectedAccountId, sessions } = get();
    if (!text.trim() || !currentSessionId) return;

    const session = sessions.find((s) => s.id === currentSessionId);
    if (!session) return;

    const userMsg: ChatMessage = {
      id: `msg_${Date.now()}_u`,
      role: 'user',
      content: text,
      attachments: attachments && attachments.length > 0 ? attachments : undefined,
      createdAt: new Date().toISOString(),
      status: 'success',
    };

    const assistantMsgId = `msg_${Date.now()}_a`;
    const assistantMsg: ChatMessage = {
      id: assistantMsgId,
      role: 'assistant',
      content: '',
      reasoning: '',
      createdAt: new Date().toISOString(),
      status: 'loading',
    };

    const updatedMessages = [...session.messages, userMsg, assistantMsg];
    const updatedSession: ChatSession = {
      ...session,
      title: session.messages.length === 0 ? text.slice(0, 18) : session.title,
      messages: updatedMessages,
      updatedAt: new Date().toISOString(),
    };

    const updatedSessions = sessions.map((s) => (s.id === currentSessionId ? updatedSession : s));
    set({ sessions: updatedSessions, isStreaming: true });

    let currentContent = '';
    let currentReasoning = '';

    await repo.sendMessageStream({
      sessionId: currentSessionId,
      content: text,
      attachments,
      model: selectedModel,
      reasoningEffort,
      accountId: selectedAccountId,
      // 上游不代管对话历史：把既有消息（历史轮）回传给网关拼进上下文。
      // 只取到 userMsg 为止，不含 assistantMsg 占位（它此刻还是空的）。
      history: [...session.messages],
      onUsage: (usage) => set({ lastUsage: usage }),
      onChunk: (chunk, reasoningChunk) => {
        if (chunk) currentContent += chunk;
        if (reasoningChunk) currentReasoning += reasoningChunk;

        const liveSessions = get().sessions.map((s) => {
          if (s.id !== currentSessionId) return s;
          const msgs = s.messages.map((m) => {
            if (m.id !== assistantMsgId) return m;
            return {
              ...m,
              content: currentContent,
              reasoning: currentReasoning,
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
              content: currentContent || '（已完成响应）',
              reasoning: currentReasoning,
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
              content: `[请求失败] ${err.message}`,
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
