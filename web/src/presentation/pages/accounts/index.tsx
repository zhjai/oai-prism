import React, { useEffect, useState, useMemo } from 'react';
import {
  Card,
  Row,
  Col,
  Table,
  Tag,
  Button,
  Space,
  Input,
  Select,
  Badge,
  Tooltip,
  message,
  Typography,
  Popconfirm,
  Empty,
  Grid,
  theme,
  Switch,
  Alert,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  SearchOutlined,
  ReloadOutlined,
  PlusOutlined,
  EyeOutlined,
  SyncOutlined,
  ClockCircleOutlined,
  EditOutlined,
  DeleteOutlined,
  TeamOutlined,
  CheckCircleOutlined,
  WarningOutlined,
  FieldTimeOutlined,
  DisconnectOutlined,
} from '@ant-design/icons';
import type { AccountStats } from '../../../domain/account/entity';
import { useAccountStore } from '../../../application/account/store';
import { useAuthStore } from '../../../application/auth/store';
import { AccountImportModal } from './AccountImportModal';
import { PlanDetailDrawer } from './PlanDetailDrawer';
import { AccountEditModal } from './AccountEditModal';
import { StatCard } from '../../components/StatCard';
import { SPECTRUM } from '../../theme/tokens';
import { formatDate } from '../../utils/format';

const { Text } = Typography;

const EXPIRING_WINDOW_SEC = 7 * 86400;

/**
 * 账号身份列：第一行是账号名称，第二行是账号 ID 与绑定邮箱。
 */
const AccountIdentity: React.FC<{ record: AccountStats }> = ({ record }) => (
  <div className="acct-ident">
    <div className="acct-ident__line">
      <span className="acct-ident__name">{record.name}</span>
      <Tag
        bordered={false}
        color={record.source === 'oauth' ? 'purple' : 'default'}
        className="acct-ident__tag"
      >
        {record.source}
      </Tag>
    </div>
    <div className="acct-ident__line acct-ident__line--meta">
      {record.id !== record.name && <span className="acct-ident__id">{record.id}</span>}
      {record.email ? (
        <Text type="secondary" className="acct-ident__email">{record.email}</Text>
      ) : (
        <Text type="secondary" className="acct-ident__email acct-ident__email--empty">未绑定邮箱</Text>
      )}
    </div>
  </div>
);

export const AccountsPage: React.FC = () => {
  const { token } = theme.useToken();
  const screens = Grid.useBreakpoint();
  // 首次渲染 breakpoint 为空对象：用 === false 判定，避免桌面端闪一次窄屏布局
  const narrow = screens.md === false;
  const {
    accounts,
    readyCount,
    loading,
    error,
    fetchAccounts,
    reloadPool,
    refreshAccount,
    deleteAccount,
    updateAccount,
    openDetailDrawer,
    openEditModal,
    setImportModalOpen,
  } = useAccountStore();
  const { blocked, openPrompt } = useAuthStore();

  // 搜索与过滤状态
  const [searchText, setSearchText] = useState('');
  const [planFilter, setPlanFilter] = useState<string>('all');
  const [statusFilter, setStatusFilter] = useState<string>('all');

  // 分页状态：只存"用户请求的页码"，实际展示的页码由过滤结果派生（见 currentPage）
  const [requestedPage, setRequestedPage] = useState(1);
  const [pageSize, setPageSize] = useState(10);

  // 进行中的动作：同一行同一时刻只允许一个动作，避免重复提交把状态改乱
  const [refreshingId, setRefreshingId] = useState<string | null>(null);
  const [togglingId, setTogglingId] = useState<string | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [reloading, setReloading] = useState(false);

  useEffect(() => {
    fetchAccounts();
  }, [fetchAccounts]);

  // 前端多维度实时过滤
  const filteredAccounts = useMemo(() => {
    return accounts.filter((acc) => {
      const matchSearch =
        !searchText.trim() ||
        acc.name.toLowerCase().includes(searchText.toLowerCase()) ||
        acc.id.toLowerCase().includes(searchText.toLowerCase()) ||
        (acc.email && acc.email.toLowerCase().includes(searchText.toLowerCase()));

      const matchPlan =
        planFilter === 'all' || acc.plan.toLowerCase() === planFilter.toLowerCase();

      const matchStatus =
        statusFilter === 'all' ||
        (statusFilter === 'enabled' && acc.available) ||
        (statusFilter === 'disabled' && !acc.enabled) ||
        (statusFilter === 'cooldown' && acc.enabled && acc.cooldown_sec > 0);

      return matchSearch && matchPlan && matchStatus;
    });
  }, [accounts, searchText, planFilter, statusFilter]);

  const filtersActive = Boolean(searchText.trim()) || planFilter !== 'all' || statusFilter !== 'all';

  const resetFilters = () => {
    setSearchText('');
    setPlanFilter('all');
    setStatusFilter('all');
    setRequestedPage(1);
  };

  // 过滤后条数变少时页码自动收回有效范围：派生而非写入 state，
  // 避免"渲染后 setState"的额外一轮渲染与警告。
  const pageCount = Math.max(1, Math.ceil(filteredAccounts.length / pageSize));
  const currentPage = Math.min(requestedPage, pageCount);

  /**
   * 导入成功后的收尾：只在筛选或页码真的偏离默认值时才写入，
   * 保持 setState 幂等（React 会跳过同值更新），也避免多一次无谓的重渲染。
   */
  const handleImported = () => {
    if (searchText) setSearchText('');
    if (planFilter !== 'all') setPlanFilter('all');
    if (statusFilter !== 'all') setStatusFilter('all');
    if (requestedPage !== 1) setRequestedPage(1);
  };

  // 账号池概览指标
  const overview = useMemo(() => {
    let cooling = 0;
    let disabled = 0;
    let inflight = 0;
    let expiring = 0;
    let earliest: string | undefined;
    for (const a of accounts) {
      if (!a.enabled) disabled++;
      else if (a.cooldown_sec > 0) cooling++;
      inflight += a.inflight;
      if (a.token_expires && (a.expires_in_sec ?? 0) > 0) {
        if ((a.expires_in_sec ?? 0) < EXPIRING_WINDOW_SEC) expiring++;
        if (!earliest || a.token_expires < earliest) earliest = a.token_expires;
      }
    }
    return { cooling, disabled, inflight, expiring, earliest };
  }, [accounts]);

  const readyPct = accounts.length > 0 ? Math.round((readyCount / accounts.length) * 100) : 0;
  // 未连接网关时数据未知：显示"—"而不是 0，避免误以为账号被清空
  const unknown = (blocked || !!error) && accounts.length === 0;
  const kpi = (n: number) => (unknown ? '—' : n);
  const unit = unknown ? undefined : '个';

  const handleReload = async () => {
    if (reloading) return;
    setReloading(true);
    try {
      await reloadPool();
      message.success('凭据已重载，账号池已刷新');
    } catch (err: any) {
      message.error(`重载失败：${err.message}`);
    } finally {
      setReloading(false);
    }
  };

  // 单账号后端刷新
  const handleSingleRefresh = async (record: AccountStats) => {
    if (refreshingId) return;
    setRefreshingId(record.id);
    try {
      const res = await refreshAccount(record.id);
      message.success(`账号 [${record.name}] 凭据已刷新！计划: ${res.plan}，到期: ${formatDate(res.expires_at)}`);
    } catch (err: any) {
      message.error(`刷新失败：${err.message}`);
    } finally {
      setRefreshingId(null);
    }
  };

  const handleToggle = async (record: AccountStats, enabled: boolean) => {
    if (togglingId) return;
    setTogglingId(record.id);
    try {
      await updateAccount(record.id, { enabled });
      message.success(`[${record.name}] 已${enabled ? '启用' : '停用'}`);
    } catch (err: any) {
      message.error(err.message || '更新失败');
    } finally {
      setTogglingId(null);
    }
  };

  const handleDelete = async (record: AccountStats) => {
    setDeletingId(record.id);
    try {
      await deleteAccount(record.id);
      message.success(`账号 [${record.name}] 已删除`);
    } catch (err: any) {
      message.error(`删除失败：${err.message}`);
    } finally {
      setDeletingId(null);
    }
  };

  const columns: ColumnsType<AccountStats> = [
    {
      title: '账号',
      dataIndex: 'name',
      key: 'name',
      // 不设宽度：吃掉其余列分完后的剩余空间
      fixed: 'left',
      render: (_, record) => <AccountIdentity record={record} />,
    },
    {
      title: '计划',
      dataIndex: 'plan',
      key: 'plan',
      width: 92,
      render: (plan) => {
        const p = (plan || 'pro').toLowerCase();
        const color = p.includes('team') || p.includes('enterprise') ? 'gold' : p.includes('pro') ? 'blue' : 'default';
        return (
          <Tag bordered={false} color={color} style={{ fontWeight: 600, marginInlineEnd: 0 }}>
            {plan || '未知'}
          </Tag>
        );
      },
    },
    {
      title: '调度状态',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 122,
      render: (enabled, record) => {
        if (!enabled) return <Badge status="default" text="已停用" />;
        if (record.cooldown_sec > 0) {
          return (
            <Tooltip title={`剩余 ${Math.round(record.cooldown_sec)} 秒后自动解除`}>
              <Badge status="warning" text={`冷却 ${Math.round(record.cooldown_sec)}s`} />
            </Tooltip>
          );
        }
        if (record.available) {
          return <Badge status="success" text="健康可用" />;
        }
        return (
          <Tooltip title={record.inflight >= record.max_concurrency && record.max_concurrency > 0 ? '并发槽位已满' : '请检查或刷新凭据'}>
            <Badge status="warning" text="暂不可用" />
          </Tooltip>
        );
      },
    },
    {
      title: '凭据构成',
      key: 'credentials',
      width: 150,
      render: (_, record) => (
        <span className="cred-tags">
          <Tooltip title={record.has_access_token ? 'Access Token 正常' : '缺失 Access Token'}>
            <Tag bordered={false} color={record.has_access_token ? 'green' : 'default'}>JWT</Tag>
          </Tooltip>
          <Tooltip title={record.has_session ? 'Session Cookie 正常' : '缺失 Session'}>
            <Tag bordered={false} color={record.has_session ? 'blue' : 'default'}>Cookie</Tag>
          </Tooltip>
          {record.has_refresh_token && (
            <Tooltip title="支持 OAuth Refresh Token 自动续签">
              <Tag bordered={false} color="purple">OAuth</Tag>
            </Tooltip>
          )}
        </span>
      ),
    },
    {
      title: '启用',
      key: 'toggle',
      width: 84,
      render: (_: unknown, record: AccountStats) => {
        const rowBusy = refreshingId === record.id || deletingId === record.id;
        return (
          <Switch
            checked={record.enabled}
            checkedChildren="启用"
            unCheckedChildren="停用"
            loading={togglingId === record.id}
            disabled={rowBusy}
            aria-label={`${record.name} 启用状态`}
            onChange={(enabled) => handleToggle(record, enabled)}
          />
        );
      },
    },
    {
      title: 'Token 到期',
      dataIndex: 'token_expires',
      key: 'token_expires',
      width: 112,
      render: (expires, record) => {
        if (!expires) return <Text type="secondary">永久/静态</Text>;
        const days = Math.round((record.expires_in_sec || 0) / 86400);
        return (
          <div style={{ lineHeight: 1.45 }}>
            <div style={{ fontVariantNumeric: 'tabular-nums' }}>{formatDate(expires)}</div>
            <Text type={days <= 2 ? 'danger' : 'secondary'} style={{ fontSize: 12 }}>
              <ClockCircleOutlined /> {days > 0 ? `剩余 ${days} 天` : '即将到期'}
            </Text>
          </div>
        );
      },
    },
    {
      // 并发与请求计数合并为一列：两行紧凑展示，窄屏也不挤压账号列
      title: <Tooltip title="在途请求 / 并发上限；本次启动以来的请求与失败数">负载</Tooltip>,
      key: 'load',
      width: 118,
      render: (_, record) => {
        const max = record.max_concurrency > 0 ? record.max_concurrency : '∞';
        return (
          <div style={{ lineHeight: 1.45, fontVariantNumeric: 'tabular-nums' }}>
            <div>
              <Text type="secondary" style={{ fontSize: 12 }}>并发 </Text>
              <strong>{record.inflight}</strong> / {max}
            </div>
            <Text type="secondary" style={{ fontSize: 12 }}>
              请求 {record.total_requests} · 失败{' '}
              <Text type={record.failures > 0 ? 'danger' : 'secondary'} style={{ fontSize: 12 }}>
                {record.failures}
              </Text>
            </Text>
          </div>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      width: 132,
      fixed: 'right',
      render: (_, record) => (
        <Space size={2}>
          <Tooltip title="明细">
            <Button
              type="text"
              size="small"
              aria-label="明细"
              icon={<EyeOutlined />}
              onClick={() => openDetailDrawer(record)}
            />
          </Tooltip>
          <Tooltip title="编辑">
            <Button
              type="text"
              size="small"
              aria-label="编辑"
              icon={<EditOutlined />}
              onClick={() => openEditModal(record)}
            />
          </Tooltip>
          <Tooltip title="刷新凭据">
            <Button
              type="text"
              size="small"
              aria-label="刷新凭据"
              icon={<SyncOutlined spin={refreshingId === record.id} />}
              disabled={refreshingId === record.id || deletingId === record.id || togglingId === record.id}
              onClick={() => void handleSingleRefresh(record)}
            />
          </Tooltip>
          <Popconfirm
            title="确定删除此账号？"
            description="将移除该账号及凭据文件中的对应记录，重载后不会恢复。"
            onConfirm={() => void handleDelete(record)}
            okText="确定删除"
            cancelText="取消"
            okButtonProps={{ danger: true, loading: deletingId === record.id }}
            disabled={deletingId !== null}
          >
            {/* 触发按钮不再套 Tooltip：浮层会在确认按钮上方持续捕获指针事件，
                让"确定删除"点不动。按钮已有 aria-label="删除"，语义不丢。 */}
            <Button
              type="text"
              danger
              size="small"
              aria-label="删除"
              title="删除"
              icon={<DeleteOutlined />}
              loading={deletingId === record.id}
            />
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <div className="page-fill acct-page" style={{ gap: 16 }}>
    {error && accounts.length > 0 && (
      <Alert
        type="error"
        showIcon
        title={`账号列表更新失败：${error}`}
        description="当前显示上次成功加载的数据。"
        action={<Button onClick={() => void fetchAccounts()} loading={loading} disabled={loading}>重试加载</Button>}
      />
    )}
    {/* 窄屏：四张指标卡各占一行、紧凑单行呈现，把视口高度让给表格；
        桌面：仍是原来的四列大卡，保持既有密度 */}
    <Row gutter={narrow ? [16, 8] : [16, 16]} style={{ flexShrink: 0 }}>
      <Col xs={24} sm={12} xl={6}>
        <StatCard
          compact={narrow}
          title="账号总数"
          value={kpi(accounts.length)}
          suffix={unit}
          icon={<TeamOutlined />}
          color={SPECTRUM[0]}
          loading={loading && accounts.length === 0}
          footer={`在途请求 ${overview.inflight}`}
        />
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <StatCard
          compact={narrow}
          title="就绪可调度"
          value={kpi(readyCount)}
          suffix={unit}
          icon={<CheckCircleOutlined />}
          color={token.colorSuccess}
          loading={loading && accounts.length === 0}
          footer={`可调度占比 ${readyPct}%`}
        />
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <StatCard
          compact={narrow}
          title="冷却 / 停用"
          value={kpi(overview.cooling + overview.disabled)}
          suffix={unit}
          icon={<WarningOutlined />}
          color={token.colorWarning}
          loading={loading && accounts.length === 0}
          footer={`冷却中 ${overview.cooling} · 已停用 ${overview.disabled}`}
        />
      </Col>
      <Col xs={24} sm={12} xl={6}>
        <StatCard
          compact={narrow}
          title="7 天内凭据到期"
          value={kpi(overview.expiring)}
          suffix={unit}
          icon={<FieldTimeOutlined />}
          color={token.colorError}
          loading={loading && accounts.length === 0}
          footer={
            overview.earliest
              ? `最早到期 ${formatDate(overview.earliest)}`
              : '无带有效期的凭据'
          }
        />
      </Col>
    </Row>
    <Card
      styles={{
        // 桌面：卡片撑满剩余高度，body 纵向 flex（工具栏固定、表格撑满、分页贴底）。
        // 窄屏：不参与填充布局 —— 卡片按内容取自然高度，否则表格会溢出到卡片边框之外。
        body: narrow
          ? { padding: '14px 14px' }
          : { padding: '16px 20px', flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' },
      }}
      style={
        narrow
          ? { width: '100%', boxShadow: token.boxShadowTertiary }
          : { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', width: '100%', boxShadow: token.boxShadowTertiary }
      }
      title={
        <Space size={8}>
          <span style={{ fontWeight: 600 }}>账号列表</span>
          <Tag bordered={false} style={{ marginInlineEnd: 0 }}>
            {filtersActive ? `${filteredAccounts.length} / ${accounts.length}` : accounts.length}
          </Tag>
        </Space>
      }
      extra={
        <Space wrap>
          <Button
            icon={<ReloadOutlined />}
            onClick={() => void handleReload()}
            loading={reloading}
            disabled={reloading}
            aria-label="重载并刷新"
          >
            重载并刷新
          </Button>
          <Button
            type="primary"
            icon={<PlusOutlined />}
            onClick={() => setImportModalOpen(true)}
          >
            导入新账号
          </Button>
        </Space>
      }
    >
      {/* 搜索与过滤工具栏（固定高度，不参与表格弹性；窄屏自动换行撑满） */}
      <div className="acct-toolbar">
        <Input
          placeholder="搜索账号 ID、名称或邮箱..."
          aria-label="搜索账号"
          prefix={<SearchOutlined style={{ color: token.colorTextQuaternary }} />}
          value={searchText}
          onChange={(e) => {
            setSearchText(e.target.value);
            setRequestedPage(1);
          }}
          style={{ width: narrow ? '100%' : 260, flex: narrow ? '1 1 100%' : undefined }}
          allowClear
        />

        <Select
          aria-label="按计划筛选"
          value={planFilter}
          onChange={(v) => {
            setPlanFilter(v);
            setRequestedPage(1);
          }}
          style={{ minWidth: narrow ? 0 : 130, flex: narrow ? '1 1 130px' : undefined }}
          options={[
            { value: 'all', label: '全部计划' },
            { value: 'pro', label: 'Pro 计划' },
            { value: 'team', label: 'Team 计划' },
            { value: 'free', label: 'Free 计划' },
          ]}
        />

        <Select
          aria-label="按调度状态筛选"
          value={statusFilter}
          onChange={(v) => {
            setStatusFilter(v);
            setRequestedPage(1);
          }}
          style={{ minWidth: narrow ? 0 : 140, flex: narrow ? '1 1 140px' : undefined }}
          options={[
            { value: 'all', label: '全部状态' },
            { value: 'enabled', label: '健康可用' },
            { value: 'cooldown', label: '冷却中' },
            { value: 'disabled', label: '已停用' },
          ]}
        />

        {filtersActive && (
          <Button type="link" size="small" onClick={resetFilters}>
            重置筛选
          </Button>
        )}
      </div>

      {/* 标准自适应分页表格：桌面撑满剩余高度（.table-fill）；窄屏随页面滚动（.table-auto） */}
      <div className={narrow ? 'table-auto' : 'table-fill'}>
        <Table
          rowKey="id"
          columns={columns}
          dataSource={filteredAccounts}
          loading={loading}
          // x = 账号列最小 240 + 其余列宽之和；容器更宽时余量全部归账号列
          // y = 视口内固定滚动区：桌面 420px（弹性容器再拉伸），窄屏由页面滚动承载
          scroll={{ x: 1048, y: narrow ? undefined : 420 }}
          locale={{
            emptyText: blocked ? (
              <Empty
                image={<DisconnectOutlined style={{ fontSize: 40, color: token.colorWarning }} />}
                styles={{ image: { height: 48 } }}
                description="未连接网关，暂时无法读取账号（数据仍在网关中）"
              >
                <Button type="primary" onClick={openPrompt}>
                  填入 API Key
                </Button>
              </Empty>
            ) : error ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={`加载失败：${error}`}>
                <Button onClick={() => void fetchAccounts()} loading={loading} disabled={loading}>
                  重试
                </Button>
              </Empty>
            ) : accounts.length === 0 ? (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有账号">
                <Button type="primary" icon={<PlusOutlined />} onClick={() => setImportModalOpen(true)}>
                  导入新账号
                </Button>
              </Empty>
            ) : (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有符合筛选条件的账号">
                <Button onClick={resetFilters}>重置筛选</Button>
              </Empty>
            ),
          }}
          pagination={{
            current: currentPage,
            pageSize: pageSize,
            total: filteredAccounts.length,
            showSizeChanger: true,
            pageSizeOptions: ['5', '10', '20', '50'],
            showQuickJumper: !narrow,
            simple: narrow,
            showTotal: (total, range) => `第 ${range[0]}-${range[1]} 条 / 共 ${total} 条账号`,
            onChange: (page, size) => {
              setRequestedPage(page);
              setPageSize(size);
            },
          }}
        />
      </div>

      <AccountImportModal onImported={handleImported} />
      <PlanDetailDrawer />
      <AccountEditModal />
    </Card>
    </div>
  );
};
