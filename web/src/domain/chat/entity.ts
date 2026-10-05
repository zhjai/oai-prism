/**
 * 对话调试领域实体与值对象
 */

export interface ChatModelInfo {
  id: string;
  name: string;
}

export type ReasoningEffort = 'low' | 'medium' | 'high' | 'xhigh';

/** 一轮推理的 token 用量（OpenAI include_usage 语义） */
export interface ChatUsage {
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
}

/** 附件（当前支持图片，走 OpenAI image_url 多模态格式，后端 translate 原生转换） */
export interface ChatAttachment {
  name: string;
  dataUrl: string; // base64 data URL
}

/** OpenAI 多模态消息内容块 */
export interface ContentPart {
  type: 'text' | 'image_url';
  text?: string;
  image_url?: { url: string };
}

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  attachments?: ChatAttachment[]; // 仅 user 消息：随消息持久化的图片附件
  reasoning?: string;         // 模型思考过程（ThoughtChain 呈现）
  progress?: string;          // 上游 agent_message 进度（仅展示与持久化，不回传为模型输入）
  status?: 'loading' | 'success' | 'error';
  createdAt: string;
}

export interface ChatSession {
  id: string;
  title: string;
  model: string;
  reasoningEffort: ReasoningEffort;
  accountId?: string;
  createdAt: string;
  updatedAt: string;
  messages: ChatMessage[];
}

export interface SendMessageOptions {
  sessionId: string;
  userMessageId: string;
  assistantMessageId: string;
  content: string;
  attachments?: ChatAttachment[];
  model: string;
  reasoningEffort: ReasoningEffort;
  accountId?: string;
  /** 本会话此前的对话历史（不含本轮 user 消息与 assistant 占位）。
   * 上游不代管对话历史，每次请求必须回传完整 messages 才有上下文。 */
  history?: ChatMessage[];
  onChunk?: (chunk: string, reasoningChunk?: string, progressChunk?: string) => void;
  onUsage?: (usage: ChatUsage) => void;
  onError?: (err: Error) => void;
  onFinish?: () => void;
}

export interface IChatRepository {
  fetchModelCatalog(): Promise<{ mains: ChatModelInfo[]; allIds: string[] }>;
  sendMessageStream(options: SendMessageOptions): Promise<void>;
  listSessions(): Promise<ChatSession[]>;
  saveSession(session: ChatSession): Promise<void>;
  deleteSession(id: string): Promise<void>;
}
