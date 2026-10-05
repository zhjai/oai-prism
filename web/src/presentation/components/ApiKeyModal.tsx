import React, { useCallback, useEffect, useState } from 'react';
import {
  Modal,
  Table,
  Button,
  Space,
  Typography,
  message,
  Input,
  Select,
  Tabs,
  Popconfirm,
  Tag,
  Tooltip,
  Empty,
  Alert,
  Checkbox,
  theme,
} from 'antd';
import {
  KeyOutlined,
  CopyOutlined,
  PlusOutlined,
  DeleteOutlined,
  CodeOutlined,
  ReloadOutlined,
  EyeOutlined,
  EyeInvisibleOutlined,
} from '@ant-design/icons';
import { getApiKey, setApiKey, httpClient } from '../../infrastructure/http/client';
import { formatDateTime } from '../utils/format';
import type { AccountStats, AdminAccountsResponse } from '../../domain/account/entity';

const { Text, Paragraph } = Typography;

interface ApiKeyModalProps {
  open: boolean;
  onClose: () => void;
}

interface ApiKeyItem {
  key: string;
  name: string;
  created_at: string;
  account_ids: string[];
  account_restricted: boolean;
}

/** 列表里默认只露出密钥前后各 4 位，需要时点“显示”就地揭示。 */
function maskKey(k: string): string {
  if (!k) return '';
  if (k.length <= 12) return `${k.slice(0, 2)}••••${k.slice(-2)}`;
  return `${k.slice(0, 6)}••••••${k.slice(-4)}`;
}

/**
 * 关闭时整棵子树卸载：密钥列表、明文揭示状态与"示例使用真实密钥"开关都随挂载
 * 生命周期归零，重新打开即干净状态 —— 不需要在 effect 里手动 setState 重置。
 */
export const ApiKeyModal: React.FC<ApiKeyModalProps> = ({ open, onClose }) => {
  if (!open) return null;
  return <ApiKeyDialog onClose={onClose} />;
};

const ApiKeyDialog: React.FC<{ onClose: () => void }> = ({ onClose }) => {
  const { token } = theme.useToken();
  const [keys, setKeys] = useState<ApiKeyItem[]>([]);
  /**
   * 初值为 true：本弹窗每次打开都重新挂载，"已挂载"就等于"还没读到列表"，
   * 表格首帧直接显示加载态与"正在读取密钥列表…"。这样挂载那次拉取只负责收尾，
   * effect 内不需要同步写状态（不产生级联渲染，也就不需要微任务/定时器绕开 lint）。
   */
  const [loading, setLoading] = useState(true);
  const [revealed, setRevealed] = useState<Set<string>>(new Set());
  const [newKeyName, setNewKeyName] = useState('');
  const [manualKey, setManualKey] = useState('');
  const [loadError, setLoadError] = useState('');
  const [savingManual, setSavingManual] = useState(false);
  const [creating, setCreating] = useState(false);
  const [accounts, setAccounts] = useState<AccountStats[]>([]);
  const [newAccountIds, setNewAccountIds] = useState<string[]>([]);
  const [editingKey, setEditingKey] = useState<ApiKeyItem | null>(null);
  const [bindingIds, setBindingIds] = useState<string[]>([]);
  const [savingBindings, setSavingBindings] = useState(false);
  const [deletingKey, setDeletingKey] = useState<string | null>(null);
  /** 代码示例是否嵌真实密钥（默认关闭：示例可能被复制或截图外传） */
  const [useRealKey, setUseRealKey] = useState(false);
  const accountOptions = accounts.map((a) => ({ value: a.id, label: `${a.name} (${a.id})${a.enabled ? '' : ' · 已停用'}` }));

  const fetchKeys = useCallback((): Promise<{ ok: boolean }> =>
    Promise.all([
        httpClient.get<ApiKeyItem[]>('/admin/apikeys'),
        httpClient.get<AdminAccountsResponse>('/admin/accounts'),
      ]).then(([res, accountRes]) => {
      const list = res.data || [];
      setKeys(list);
      setAccounts(accountRes.data.accounts);
      setLoadError('');
      return { ok: true };
      }).catch((err: any) => {
      // 拉取失败保留上一次成功的数据（含刚生成时已并入的那条），只有本来就没数据才空态
      setLoadError(err?.message || '加载失败');
      return { ok: false };
      }).finally(() => {
        setLoading(false);
      }), []);

  // 挂载即加载一次（父组件在 open 时才挂载本组件，关闭即卸载）
  useEffect(() => {
    void fetchKeys();
  }, [fetchKeys]);

  /** 显式刷新才置加载态；其余流程由各自的 saving/deleting/creating 状态表达进度 */
  const handleReload = () => {
    setLoading(true);
    void fetchKeys();
  };

  const handleCreate = async () => {
    if (creating) return;
    setCreating(true);
    try {
      const res = await httpClient.post<ApiKeyItem>('/admin/apikeys', { name: newKeyName || '新访问密钥', account_ids: newAccountIds });
      const created = res.data;
      if (!created?.key) {
        // 后端没回明文 Key：不能谎报成功，也不留下一个查不到的"已生成"
        message.error('生成失败：服务端未返回新密钥，请在列表中确认后重试');
        return;
      }
      // 绑定信息以本次请求发出的内容为准：响应缺字段时不能默认成"全部启用账号"，
      // 否则列表会把有绑定的密钥说成无绑定。
      const sentIds = newAccountIds;
      const normalized: ApiKeyItem = {
        ...created,
        account_ids: created.account_ids ?? sentIds,
        account_restricted:
          typeof created.account_restricted === 'boolean' ? created.account_restricted : sentIds.length > 0,
      };
      setNewKeyName('');
      setNewAccountIds([]);
      // 立刻进可见清单：默认仍是掩码显示，复制按钮写的是原始值，不需要点"显示"。
      // 这样即使刷新失败、或浏览器已有凭据（不采用这把新 Key），明文也不会丢。
      setKeys((prev) => (prev.some((item) => item.key === normalized.key) ? prev : [normalized, ...prev]));
      // 首次生成时先建立凭据，再刷新列表；保留已有浏览器管理凭据。
      if (!getApiKey()) {
        try {
          await httpClient.get('/admin/stats', { headers: { Authorization: `Bearer ${created.key}` } });
          setApiKey(created.key);
        } catch {
          // 新 Key 保留在掩码列表中，供稍后恢复。
        }
      }
      const { ok } = await fetchKeys();
      if (ok) {
        message.success(
          getApiKey() === created.key ? '已生成新的对外 API 密钥，并设为当前浏览器使用的密钥' : '已生成新的对外 API 密钥',
        );
      } else {
        // 不虚报，也不把明文塞进输入框：密钥已在清单里，可复制、可设为默认
        message.warning('新密钥已生成，暂时读不到密钥列表；已保留在列表中，可复制或设为默认');
      }
    } catch (err: any) {
      message.error(`生成失败：${err.message}`);
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async (k: string) => {
    if (deletingKey) return;
    setDeletingKey(k);
    try {
      await httpClient.delete(`/admin/apikeys/${encodeURIComponent(k)}`);
      // 删掉的正好是本浏览器在用的凭据时：改用清单里剩下的第一把，没有就清空
      // （本轮本机管理凭据随之失效，页面会给出"未连接"引导，而不是继续发 401）
      if (getApiKey() === k) setApiKey(keys.find((item) => item.key !== k)?.key || '');
      message.success('已注销该密钥');
      await fetchKeys();
    } catch (err: any) {
      message.error(`删除失败：${err.message}`);
    } finally {
      setDeletingKey(null);
    }
  };

  const handleSaveBindings = async () => {
    if (!editingKey || savingBindings) return;
    setSavingBindings(true);
    try {
      await httpClient.put(`/admin/apikeys/${encodeURIComponent(editingKey.key)}/bindings`, { account_ids: bindingIds });
      message.success('账号绑定已保存并生效');
      setEditingKey(null);
      // 重新写入当前 Key（触发凭据变更刷新），保留管理凭据不被删除动作清掉
      if (getApiKey() === editingKey.key) setApiKey(editingKey.key);
      await fetchKeys();
    } catch (err: any) {
      message.error(err.message || '绑定保存失败');
    } finally {
      setSavingBindings(false);
    }
  };

  /**
   * 填入已有 Key：先带显式 Authorization 对 /admin/stats 验一次，通过才落盘。
   * 校验失败时不写入本地、也不清空输入框 —— 避免一个手滑的粘贴顶掉正在生效的
   * 管理凭据，同时保留原文便于改正。
   */
  const handleUseManualKey = async () => {
    const k = manualKey.trim();
    if (!k || savingManual) return;
    setSavingManual(true);
    try {
      await httpClient.get('/admin/stats', { headers: { Authorization: `Bearer ${k}` } });
    } catch (err: any) {
      message.error(`这个 Key 无法通过网关校验：${err?.message || '请检查是否复制完整'}`);
      return;
    } finally {
      setSavingManual(false);
    }
    // 验过了才覆盖本地凭据（原有的 Key 或管理员会话令牌）
    setApiKey(k);
    setManualKey('');
    message.success('已保存到浏览器，之后的请求将携带此密钥');
    // 刷新失败由 handleReload / 告警里的重试按钮承担，这里不再介入加载态
    await fetchKeys();
  };

  /**
   * 唯一的剪贴板写入口：await 结果，成功/失败各给一次提示。
   * 失败时不重试同一个不可用的 API，也不虚报成功。
   */
  const writeClipboard = async (text: string, successText: string): Promise<boolean> => {
    try {
      await navigator.clipboard.writeText(text);
      message.success(successText);
      return true;
    } catch {
      message.error('复制失败：浏览器拒绝了剪贴板访问，请手动选中后复制');
      return false;
    }
  };

  /** 复制示例代码：示例里若嵌了真实密钥，提示里说明这一点。 */
  const handleCopyExample = (code: string) => {
    void writeClipboard(code, useRealKey ? '已复制示例，其中包含真实密钥' : '已复制到剪贴板');
  };

  const toggleReveal = (k: string) => {
    setRevealed((prev) => {
      const next = new Set(prev);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  };

  // 明文只由"显示/隐藏"决定：复制不改变可见性
  const isRevealed = (k: string) => revealed.has(k);

  const columns = [
    {
      title: '密钥名称',
      dataIndex: 'name',
      key: 'name',
      render: (name: string) => <Text strong>{name}</Text>,
    },
    {
      title: 'API Key (Bearer Token)',
      dataIndex: 'key',
      key: 'key',
      render: (k: string) => (
        <Space size={2}>
          <Text code style={{ fontSize: 12 }}>{isRevealed(k) ? k : maskKey(k)}</Text>
          <Tooltip title={isRevealed(k) ? '隐藏' : '显示完整密钥'}>
            <Button
              type="text"
              size="small"
              aria-label={isRevealed(k) ? '隐藏密钥' : '显示密钥'}
              icon={isRevealed(k) ? <EyeInvisibleOutlined /> : <EyeOutlined />}
              onClick={() => toggleReveal(k)}
            />
          </Tooltip>
          <Tooltip title="复制">
            <Button
              type="text"
              size="small"
              aria-label="复制密钥"
              icon={<CopyOutlined />}
              onClick={() => void writeClipboard(k, '已复制到剪贴板')}
            />
          </Tooltip>
        </Space>
      ),
    },
    {
      title: '绑定账号',
      key: 'bindings',
      render: (_: unknown, r: ApiKeyItem) => r.account_restricted
        ? r.account_ids.length > 0
          ? <Space size={4} wrap>{r.account_ids.map((id) => <Tag key={id}>{accounts.find((a) => a.id === id)?.name || id}</Tag>)}</Space>
          : <Text type="warning">无可用绑定账号</Text>
        : <Text type="secondary">全部启用账号</Text>,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      render: (t: string) => (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {formatDateTime(t)}
        </Text>
      ),
    },
    {
      title: '操作',
      key: 'action',
      width: 232,
      render: (_: any, r: ApiKeyItem) => (
        <Space size={2}>
          <Tooltip title="选择这个密钥可以使用哪些账号">
            <Button type="text" size="small" onClick={() => { setEditingKey(r); setBindingIds(r.account_ids || []); }}>
              绑定账号
            </Button>
          </Tooltip>
          {/* 与账号删除同一处理：触发按钮不套 Tooltip，浮层会挡住确认按钮的点击。
              行内按钮本身已是自解释文案，图标按钮带了 aria-label 与 title。 */}
          <Popconfirm
            title="设置当前使用的密钥？"
            description="页面内所有请求将改用此密钥（保存在浏览器本地）。"
            onConfirm={() => {
              setApiKey(r.key);
              message.success('已设为当前使用的密钥');
            }}
            okText="确定"
            cancelText="取消"
          >
            <Button type="text" size="small" icon={<KeyOutlined />} title="设为本浏览器管理请求使用的密钥">
              {getApiKey() === r.key ? '使用中' : '设为默认'}
            </Button>
          </Popconfirm>
          <Popconfirm
            title="确定注销此密钥？"
            description="注销后使用此 Key 的外部客户端将无法调用。"
            onConfirm={() => void handleDelete(r.key)}
            okText="确定"
            cancelText="取消"
            okButtonProps={{ danger: true, loading: deletingKey === r.key }}
            disabled={deletingKey !== null}
          >
            <Button
              type="text"
              danger
              size="small"
              aria-label="注销密钥"
              title="注销密钥"
              icon={<DeleteOutlined />}
              loading={deletingKey === r.key}
            />
          </Popconfirm>
        </Space>
      ),
    },
  ];

  // 示例默认用占位符：真实密钥需要用户显式勾选后才嵌入
  const firstKey = useRealKey
    ? getApiKey() || keys[0]?.key || '<你的 API Key>'
    : '<你的 API Key>';

  const curlExample = `curl http://localhost:8787/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${firstKey}" \\
  -d '{
    "model": "gpt-6.1-sol",
    "messages": [{"role": "user", "content": "你好！"}]
  }'`;

  const pythonExample = `from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8787/v1",
    api_key="${firstKey}"
)

resp = client.chat.completions.create(
    model="gpt-6.1-sol",
    messages=[{"role": "user", "content": "你好！"}]
)
print(resp.choices[0].message.content)`;

  const codexExample = `# ~/.codex/config.toml
model_provider = "oaiprism"
model = "gpt-6.1-sol"
model_context_window = 16384

[model_providers.oaiprism]
name = "oaiprism"
wire_api = "responses"
requires_openai_auth = false
base_url = "http://localhost:8787/v1"
experimental_bearer_token = "${firstKey}"`;

  const preStyle: React.CSSProperties = {
    background: 'var(--op-code-bg)',
    border: `1px solid ${token.colorBorderSecondary}`,
    padding: 12,
    borderRadius: 8,
    fontSize: 12,
    marginTop: 6,
    overflowX: 'auto',
  };

  const exampleBlock = (label: string, code: string, copyLabel?: string) => (
    <div style={{ marginTop: 16 }}>
      <Text strong><CodeOutlined /> {label}</Text>
      <pre style={preStyle}>{code}</pre>
      {copyLabel && (
        <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyExample(code)}>
          {copyLabel}
        </Button>
      )}
    </div>
  );

  return (
    <>
    <Modal
      title={
        <Space>
          <KeyOutlined style={{ color: token.colorPrimary }} />
          <span>对外 API Key 管理与接入指引</span>
        </Space>
      }
      open
      onCancel={onClose}
      footer={[
        <Button key="close" type="primary" onClick={onClose}>
          完成
        </Button>,
      ]}
      width={1000}
      destroyOnHidden
    >
      <Tabs
        defaultActiveKey="keys"
        items={[
          {
            key: 'keys',
            label: '密钥列表',
            children: (
              <div>
                {loadError && (
                  <Alert
                    type={keys.length === 0 ? 'error' : 'warning'}
                    showIcon
                    title={keys.length === 0 ? `无法读取密钥列表：${loadError}` : `密钥列表暂时无法刷新：${loadError}`}
                    description={
                      keys.length === 0
                        ? '网关需要有效凭据才能列出密钥。若这是全新网关，请用上方输入框填入刚生成的密钥，或改用管理员密码登录。'
                        : '下方显示的是最近一次成功读取到的内容，其中包含本次新生成的密钥，仍可复制或设为默认。'
                    }
                    action={
                      <Button size="small" icon={<ReloadOutlined />} onClick={handleReload} loading={loading} disabled={loading}>
                        重试加载
                      </Button>
                    }
                    style={{ marginBottom: 12 }}
                  />
                )}

                <div style={{ display: 'flex', gap: 12, marginBottom: 12, flexWrap: 'wrap' }}>
                  <Input.Password
                    placeholder="已有 API Key：粘贴后保存到本浏览器"
                    aria-label="填入已有 API Key"
                    value={manualKey}
                    onChange={(e) => setManualKey(e.target.value)}
                    onPressEnter={() => void handleUseManualKey()}
                    style={{ flex: '1 1 260px' }}
                  />
                  <Button
                    icon={<KeyOutlined />}
                    onClick={() => void handleUseManualKey()}
                    loading={savingManual}
                    disabled={savingManual || !manualKey.trim()}
                  >
                    使用此 Key
                  </Button>
                </div>

                <div style={{ display: 'flex', gap: 12, marginBottom: 12, flexWrap: 'wrap' }}>
                  <Input
                    placeholder="新 Key 描述名称，例如：生产环境客户端"
                    aria-label="新密钥名称"
                    value={newKeyName}
                    onChange={(e) => setNewKeyName(e.target.value)}
                    onPressEnter={() => void handleCreate()}
                    style={{ flex: '1 1 260px' }}
                  />
                  <Button
                    type="primary"
                    icon={<PlusOutlined />}
                    onClick={() => void handleCreate()}
                    loading={creating}
                    disabled={creating}
                  >
                    生成新 Key
                  </Button>
                </div>

                <Select
                  mode="multiple"
                  aria-label="新密钥绑定账号"
                  placeholder="绑定一个或多个账号；留空使用全部启用账号"
                  value={newAccountIds}
                  onChange={setNewAccountIds}
                  options={accountOptions}
                  optionFilterProp="label"
                  disabled={creating}
                  style={{ width: '100%' }}
                />

                <Table
                  rowKey="key"
                  columns={columns}
                  dataSource={keys}
                  loading={loading}
                  pagination={false}
                  size="small"
                  scroll={{ x: 780 }}
                  locale={{
                    emptyText: loading ? (
                      <span style={{ display: 'inline-block', padding: '16px 0' }}>正在读取密钥列表…</span>
                    ) : loadError ? (
                      <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="密钥列表暂不可读" />
                    ) : (
                      <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有对外密钥" />
                    ),
                  }}
                  style={{ marginTop: 12 }}
                />
              </div>
            ),
          },
          {
            key: 'quickstart',
            label: '客户端接入代码示例',
            children: (
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 4 }}>
                  <Checkbox
                    checked={useRealKey}
                    onChange={(e) => setUseRealKey(e.target.checked)}
                    disabled={!getApiKey() && keys.length === 0}
                  >
                    在示例中使用真实密钥
                  </Checkbox>
                  {useRealKey && (
                    <Tooltip title="复制到剪贴板">
                      <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyExample(firstKey)}>
                        复制密钥
                      </Button>
                    </Tooltip>
                  )}
                </div>

                {exampleBlock('1. cURL 命令行调用示例：', curlExample, '复制 cURL 示例')}
                {exampleBlock('2. Python (openai-python) 接入：', pythonExample)}
                {exampleBlock('3. Codex CLI 接入（config.toml）：', codexExample)}
              </div>
            ),
          },
        ]}
      />
    </Modal>
    <Modal
      title={editingKey ? `绑定账号 · ${editingKey.name}` : '绑定账号'}
      open={Boolean(editingKey)}
      onCancel={() => { if (!savingBindings) setEditingKey(null); }}
      onOk={handleSaveBindings}
      confirmLoading={savingBindings}
      okText="保存绑定"
      cancelText="取消"
      closable={!savingBindings}
      keyboard={!savingBindings}
      cancelButtonProps={{ disabled: savingBindings }}
      width={520}
      destroyOnHidden
    >
      <Paragraph type="secondary">
        支持绑定多个账号，请求只在绑定的启用账号中调度。清空并保存后可使用全部启用账号。
      </Paragraph>
      <Select
        mode="multiple"
        aria-label="绑定账号选择"
        placeholder="选择一个或多个账号"
        value={bindingIds}
        onChange={setBindingIds}
        options={accountOptions}
        optionFilterProp="label"
        disabled={savingBindings}
        style={{ width: '100%' }}
      />
      {accounts.length === 0 && (
        <Text type="warning" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
          暂时读不到账号列表，无法在此选择绑定账号；可稍后重试。
        </Text>
      )}
    </Modal>
    </>
  );
};
