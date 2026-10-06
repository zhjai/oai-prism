/**
 * 模型清单工具 —— 后端 /v1/models 已只暴露现役模型（按账号动态获取），
 * 前端不再做别名/下线过滤，只保留两条简单规则：
 *   1. 主模型 = 不带推理档位后缀的条目（-low/-high/-xhigh 是同一模型的档位变体）
 *   2. 各主模型的可用档位 = 该模型的 effort 变体是否存在于清单（上游目录与有效别名为准）
 */

/** effort 变体：gpt-5.6-sol-high → true */
export const isEffortVariant = (id: string) => /-(low|medium|high|xhigh)$/.test(id);

/** 剥离推理档位后缀：gpt-5.6-sol-high → gpt-5.6-sol */
export const stripEffort = (id: string) => id.replace(/-(low|medium|high|xhigh)$/, '');

/** 主模型条目（不含档位后缀）。历史数据里的旧模型用 isMainModelId 兜底过滤。 */
export const pickMainModels = <T extends { id: string; upstream_model?: string }>(list: T[]): T[] =>
  list.filter((m) => m.upstream_model ? m.id === m.upstream_model : !isEffortVariant(m.id));

/** 主模型命名模式：gpt-<版本>-<系列名>（统计页过滤 SQLite 历史旧模型时兜底） */
export const isMainModelId = (id: string) => /^gpt-\d+(\.\d+)?-[a-z]+$/.test(id);

/** 版本号解析：gpt-5.6-sol → [5, 6]；gpt-6-luna → [6, 0] */
export const modelVersion = (id: string): [number, number] => {
  const m = id.match(/^gpt-(\d+)(?:\.(\d+))?-/);
  return m ? [Number(m[1]), Number(m[2] || '0')] : [0, 0];
};

/** 排序：新版本在前（6.1 → 6 → 5.6），同版本按 id 字母序 */
export const compareModelNewestFirst = (
  a: { id: string },
  b: { id: string },
): number => {
  const [bm, bn] = modelVersion(b.id);
  const [am, an] = modelVersion(a.id);
  return bm - am || bn - an || a.id.localeCompare(b.id);
};

/** 推理档位（与后端 reasoning_effort 参数一致） */
export type ModelEffort = string;

/**
 * 从 /v1/models 全量 id 清单推导某主模型的可用推理档位。
 * 各模型的档位由后端配置决定（如 6 Luna 没有 low），
 * 以「该模型的 -low/-high/-xhigh 变体是否存在」为准，零硬编码。
 */
export const effortsForModel = (baseId: string, allIds: string[], advertised?: ModelEffort[]): ModelEffort[] => {
  if (advertised?.length) return advertised;
  const has = (suffix: string) => allIds.includes(baseId + suffix);
  return [
    ...(has('-low') ? (['low'] as ModelEffort[]) : []),
    'medium',
    ...(has('-high') ? (['high'] as ModelEffort[]) : []),
    ...(has('-xhigh') ? (['xhigh'] as ModelEffort[]) : []),
  ];
};
