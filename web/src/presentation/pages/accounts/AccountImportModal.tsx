import React, { useEffect, useRef, useState } from 'react';
import {
  Alert,
  Modal,
  Tabs,
  Input,
  Upload,
  message,
  Typography,
  Space,
  Button,
  Form,
  InputNumber,
  Select,
  Switch,
  Collapse,
  Checkbox,
  Row,
  Col,
} from 'antd';
import {
  InboxOutlined,
  KeyOutlined,
  FileTextOutlined,
  SafetyCertificateOutlined,
  LinkOutlined,
  UserAddOutlined,
} from '@ant-design/icons';
import { httpClient } from '../../../infrastructure/http/client';
import { useAccountStore } from '../../../application/account/store';
import type { AccountConfig } from '../../../domain/account/entity';

const { TextArea } = Input;
const { Dragger } = Upload;
const { Text } = Typography;

/** 官方 OAuth 授权导入：浏览器走 auth.openai.com 官方授权页，本地回调接 code 换 token。 */
const OAuthImportPane: React.FC<{ onDone: () => void }> = ({ onDone }) => {
  const [phase, setPhase] = useState<'idle' | 'waiting' | 'success' | 'error'>('idle');
  const [sessionId, setSessionId] = useState('');
  const [errMsg, setErrMsg] = useState('');
  const [callbackUrl, setCallbackUrl] = useState('');
  const [exchanging, setExchanging] = useState(false);
  const [beginning, setBeginning] = useState(false);
  const timerRef = useRef<number | null>(null);

  // 离开弹窗/组件卸载时停掉轮询
  useEffect(() => {
    return () => {
      if (timerRef.current) window.clearInterval(timerRef.current);
    };
  }, []);

  const begin = async () => {
    if (beginning || phase === 'waiting') return;
    setBeginning(true);
    try {
      const res = await httpClient.post<any>('/admin/oauth/begin', {
        redirect_uri: 'http://localhost:1455/auth/callback',
      });
      const { session_id, authorize_url } = res.data || {};
      if (!session_id || !authorize_url) {
        message.error('后端未返回授权地址');
        return;
      }
      setSessionId(session_id);
      setPhase('waiting');
      window.open(authorize_url, '_blank', 'noopener');

      // 轮询导入进度（回调监听器自动完成时结束）
      timerRef.current = window.setInterval(async () => {
        try {
          const st = await httpClient.get<any>(`/admin/oauth/status`, {
            params: { session_id },
          });
          const status = st.data?.status;
          if (status === 'success') {
            window.clearInterval(timerRef.current!);
            setPhase('success');
            message.success(`授权完成，账号已入库（${st.data.account_id}）`);
            onDone();
          } else if (status === 'error') {
            window.clearInterval(timerRef.current!);
            setErrMsg(st.data?.error || '未知错误');
            setPhase('error');
          }
        } catch {
          // 单次轮询失败忽略，下个周期重试
        }
      }, 2000);
    } catch (err: any) {
      message.error(err.message || '发起授权失败');
    } finally {
      setBeginning(false);
    }
  };

  const exchangeManual = async () => {
    if (!callbackUrl.trim()) {
      message.warning('请粘贴授权后浏览器跳转的完整回调地址');
      return;
    }
    if (exchanging) return;
    setExchanging(true);
    try {
      const res = await httpClient.post<any>('/admin/oauth/exchange', {
        session_id: sessionId,
        callback: callbackUrl.trim(),
      });
      message.success(`授权完成，账号已入库（${res.data?.account_id}）`);
      setPhase('success');
      onDone();
    } catch (err: any) {
      message.error(err.response?.data?.error || err.message || '换 token 失败');
    } finally {
      setExchanging(false);
    }
  };

  if (phase === 'success') {
    return (
      <Alert
        type="success"
        showIcon
        title="授权完成"
        description="账号已写入 SQLite 并进入调度池，可在列表中查看。"
      />
    );
  }

  return (
    <Space orientation="vertical" style={{ width: '100%' }} size="middle">
      <Text type="secondary">
        跳转到 auth.openai.com 官方授权页登录（与 Codex CLI 同款 PKCE 流程），
        授权完成后本地回调端口自动换取 token 并入库，全程无需手动复制凭据。
      </Text>

      {phase === 'waiting' && (
        <Alert
          type="info"
          showIcon
          title="等待授权完成…"
          description="已在浏览器打开官方授权页。若本地回调端口（1455）被占用，可把授权完成后浏览器地址栏里的完整回调地址粘贴到下方手动完成导入。"
        />
      )}

      {phase === 'error' && (
        <Alert
          type="error"
          showIcon
          title="导入失败"
          description={errMsg}
          action={
            <Button size="small" onClick={() => { setErrMsg(''); setPhase('idle'); }}>
              重新授权
            </Button>
          }
        />
      )}

      <div>
        <Button
          type="primary"
          icon={<SafetyCertificateOutlined />}
          onClick={begin}
          loading={beginning}
          disabled={beginning || phase === 'waiting'}
        >
          打开官方授权页
        </Button>
      </div>

      {(phase === 'waiting' || phase === 'error') && (
        <div>
          <Text type="secondary" style={{ fontSize: 12 }}>
            手动兜底：粘贴授权后浏览器跳转的完整地址（含 code 参数）：
          </Text>
          <Space.Compact style={{ width: '100%', marginTop: 6 }}>
            <Input
              prefix={<LinkOutlined />}
              aria-label="OAuth 回调地址"
              placeholder="http://localhost:1455/auth/callback?code=...&state=..."
              value={callbackUrl}
              onChange={(e) => setCallbackUrl(e.target.value)}
            />
            <Button onClick={exchangeManual} loading={exchanging} disabled={exchanging}>
              完成导入
            </Button>
          </Space.Compact>
        </div>
      )}
    </Space>
  );
};

interface AccountImportModalProps {
  /** 导入成功后回调（父级用它重置筛选与页码，避免新账号被旧筛选挡住） */
  onImported?: () => void;
}

export const AccountImportModal: React.FC<AccountImportModalProps> = ({ onImported }) => {
  const { importModalOpen, setImportModalOpen, importAccounts, fetchAccounts } = useAccountStore();
  const [form] = Form.useForm<AccountConfig>();
  const [activeTab, setActiveTab] = useState('direct');
  const [rawText, setRawText] = useState('');
  const [verify, setVerify] = useState(true);
  const [loading, setLoading] = useState(false);

  const clearInputs = () => {
    form.resetFields();
    setRawText('');
    setVerify(true);
    setActiveTab('direct');
  };

  const handleCancel = () => {
    if (loading) return;
    clearInputs();
    setImportModalOpen(false);
  };

  const handleOk = async () => {
    if (loading) return;
    // OAuth 页由面板自己的按钮驱动，回车不应落到批量导入分支
    if (activeTab === 'oauth') return;
    if (activeTab === 'direct') {
      // 表单校验失败时 validateFields 抛出：字段下方就地显示错误，这里不弹全局提示
      let values: AccountConfig;
      try {
        values = await form.validateFields();
      } catch {
        return;
      }
      const account: AccountConfig = {
        ...values,
        name: values.name?.trim(),
        id: values.id?.trim() || undefined,
        cookies: values.cookies?.trim(),
        proxy: values.proxy?.trim() || undefined,
        access_token: values.access_token?.trim() || undefined,
        refresh_token: values.refresh_token?.trim() || undefined,
      };
      setLoading(true);
      try {
        await importAccounts({ accounts: [account], verify });
        message.success(`账号 [${account.name}] 导入成功`);
        clearInputs();
        setImportModalOpen(false);
        onImported?.();
      } catch (err: any) {
        message.error(err.message || '导入失败，请检查数据格式');
      } finally {
        setLoading(false);
      }
      return;
    }

    if (!rawText.trim()) {
      message.warning('请先输入或上传账号凭据内容');
      return;
    }
    setLoading(true);
    try {
      await importAccounts({ rawText, verify });
      message.success('账号导入成功');
      clearInputs();
      setImportModalOpen(false);
      onImported?.();
    } catch (err: any) {
      // 解析/校验错误保留输入内容，便于就地修正后重试
      message.error(err.message || '导入失败，请检查数据格式');
    } finally {
      setLoading(false);
    }
  };

  const handleOAuthDone = () => {
    fetchAccounts();
    clearInputs();
    setImportModalOpen(false);
    onImported?.();
  };

  const items = [
    {
      key: 'direct',
      label: <span><UserAddOutlined /> 直接添加</span>,
      children: (
        <Form
          form={form}
          layout="vertical"
          disabled={loading}
          initialValues={{ name: '', cookies: '', plan: '', max_concurrency: 2, enabled: true }}
          autoComplete="off"
          requiredMark
        >
          <Form.Item
            name="name"
            label="账号名称"
            rules={[{ required: true, whitespace: true, message: '请输入账号名称' }]}
          >
            <Input aria-label="账号名称" placeholder="例如：团队主力账号" maxLength={64} />
          </Form.Item>

          <Form.Item
            name="id"
            label="账号 ID（可选）"
            // 空值合法：pattern 不校验空字符串，只拦带空格的 ID
            rules={[{ pattern: /^\S+$/, message: '账号 ID 不能包含空格' }]}
          >
            <Input aria-label="账号 ID（可选）" placeholder="留空自动生成" maxLength={64} />
          </Form.Item>

          <Form.Item
            name="cookies"
            label="完整 Cookie"
            rules={[{ required: true, whitespace: true, message: '请输入完整 Cookie' }]}
          >
            <TextArea
              aria-label="完整 Cookie"
              autoSize={{ minRows: 4, maxRows: 8 }}
              autoComplete="off"
              spellCheck={false}
              placeholder="prism_oai_access_token=eyJ...; prism_session_token=..."
            />
          </Form.Item>

          <Row gutter={12}>
            <Col xs={24} sm={10}>
              <Form.Item
                name="max_concurrency"
                label="最大并发数（0 = 不限）"
                rules={[
                  { required: true, message: '请输入最大并发数' },
                  { type: 'integer', min: 0, message: '并发数必须是非负整数' },
                ]}
              >
                <InputNumber aria-label="最大并发数（0 = 不限）" min={0} precision={0} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={14}>
              <Form.Item
                name="plan"
                label="计划标签"
                extra="仅本地标记，不影响上游订阅"
              >
                <Select
                  aria-label="计划标签"
                  options={[
                    { value: '', label: '自动识别' },
                    { value: 'free', label: 'Free' },
                    { value: 'plus', label: 'Plus' },
                    { value: 'prolite', label: 'Pro Lite' },
                    { value: 'pro', label: 'Pro' },
                    { value: 'team', label: 'Team' },
                    { value: 'enterprise', label: 'Enterprise' },
                  ]}
                />
              </Form.Item>
            </Col>
          </Row>

          <Form.Item
            name="enabled"
            label="启用账号"
            valuePropName="checked"
            style={{ marginBottom: 8 }}
          >
            <Switch aria-label="启用账号" />
          </Form.Item>

          <Collapse
            ghost
            items={[{
              key: 'advanced',
              label: '高级设置（可选）',
              children: <>
                <Form.Item name="proxy" label="代理地址">
                  <Input aria-label="代理地址" placeholder="http://127.0.0.1:7890" />
                </Form.Item>
                <Form.Item name="access_token" label="Access Token">
                  <Input.Password aria-label="Access Token" autoComplete="off" />
                </Form.Item>
                <Form.Item name="refresh_token" label="Refresh Token">
                  <Input.Password aria-label="Refresh Token" autoComplete="off" />
                </Form.Item>
              </>,
            }]}
          />
        </Form>
      ),
    },
    {
      key: 'oauth',
      label: (
        <span>
          <SafetyCertificateOutlined /> 官方授权登录
        </span>
      ),
      children: <OAuthImportPane onDone={handleOAuthDone} />,
    },
    {
      key: 'text',
      label: (
        <span>
          <KeyOutlined /> 文本 / JSON 粘贴
        </span>
      ),
      children: (
        <Space orientation="vertical" style={{ width: '100%' }}>
          <TextArea
            rows={8}
            aria-label="批量账号凭据"
            autoComplete="off"
            spellCheck={false}
            disabled={loading}
            placeholder={'prism_oai_access_token=eyJ...; prism_session_token=...\n或\n[{"name":"Team通道","cookie":"prism_oai_access_token=...; prism_session_token=...","plan":"Enterprise"}]'}
            value={rawText}
            onChange={(e) => setRawText(e.target.value)}
          />
        </Space>
      ),
    },
    {
      key: 'file',
      label: (
        <span>
          <FileTextOutlined /> 文件拖拽导入
        </span>
      ),
      children: (
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Dragger
            accept=".json,.txt"
            showUploadList={false}
            disabled={loading}
            beforeUpload={(file) => {
              const reader = new FileReader();
              reader.onload = (e) => {
                const content = e.target?.result as string;
                if (content) {
                  // 内容落到"文本 / JSON 粘贴"页：确认后再提交导入
                  setRawText(content);
                  setActiveTab('text');
                }
              };
              reader.onerror = () => message.error('文件读取失败，请重试');
              reader.readAsText(file);
              return false;
            }}
          >
            <p className="ant-upload-drag-icon">
              <InboxOutlined />
            </p>
            <p className="ant-upload-text">点击或拖拽 accounts.json 或 cookie.txt 文件到此区域</p>
          </Dragger>
        </Space>
      ),
    },
  ];

  const directSubmitDisabled = loading;

  return (
    <Modal
      title="导入账号凭据"
      open={importModalOpen}
      onOk={handleOk}
      confirmLoading={loading}
      onCancel={handleCancel}
      closable={!loading}
      maskClosable={!loading}
      keyboard={!loading}
      width={620}
      okText="确认导入"
      cancelText="取消"
      cancelButtonProps={{ disabled: loading }}
      /*
       * 主按钮的可访问名固定为"确认导入"：antd 在 loading/confirmLoading 时会插入
       * 一个 role="img" aria-label="loading" 的旋转图标，按内容计算出来的名字会变成
       * "loading 确认导入"，按精确名定位的回归用例就会在按钮可见的情况下失配。
       * 显式 aria-label 让名字与加载状态解耦（可见文本仍是"确认导入"，符合
       * WCAG 2.5.3 标签与名称一致），加载/禁用状态另用 aria-busy 与 disabled 表达，
       * 不靠图标污染名字。此处只改语义标注，不动任何尺寸与位置。
       */
      okButtonProps={
        activeTab === 'oauth'
          ? { style: { display: 'none' }, 'aria-label': '确认导入', 'aria-busy': loading }
          : { disabled: directSubmitDisabled, 'aria-label': '确认导入', 'aria-busy': loading }
      }
      destroyOnHidden
    >
      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={items.map((item) => ({ ...item, disabled: loading }))}
      />
      {activeTab !== 'oauth' && (
        <Space orientation="vertical" style={{ width: '100%', marginTop: 16 }} size={8}>
          <Checkbox checked={verify} onChange={(event) => setVerify(event.target.checked)} disabled={loading}>
            导入前校验凭据
          </Checkbox>
          {!verify && <Alert type="warning" showIcon title="已跳过校验，仅保存凭据；账号可能暂不可用。" />}
        </Space>
      )}
    </Modal>
  );
};
