import React, { useEffect, useState } from 'react';
import { Alert, Modal, Form, Input, Select, InputNumber, Row, Col, Typography } from 'antd';
import { useAccountStore } from '../../../application/account/store';
import type { AccountStats } from '../../../domain/account/entity';

const { Text } = Typography;

interface EditFormShape {
  name: string;
  email?: string;
  plan?: string;
  max_concurrency: number;
}

/** 表单初值：由账号数据派生，换账号时随重新挂载重建，不经 effect。 */
const toFormValues = (account: AccountStats): EditFormShape => ({
  name: account.name,
  email: account.email,
  // 保留空计划：空值表示自动识别，不在编辑时被静默改成 pro
  plan: account.plan ?? '',
  max_concurrency: account.max_concurrency ?? 2,
});

/**
 * 编辑账号：名称、邮箱、计划标记与并发槽位。
 * 保存失败时留在弹窗内就地提示并可重试；保存中禁用重复提交。
 */
export const AccountEditModal: React.FC = () => {
  const { editingAccount, editModalOpen, closeEditModal, updateAccount } = useAccountStore();

  if (!editingAccount) return null;

  return (
    <EditAccountDialog
      // 换一条账号即换 key：表单、错误与保存中状态整体重建
      key={editingAccount.id}
      account={editingAccount}
      open={editModalOpen}
      onCancel={closeEditModal}
      onSubmit={(values) => updateAccount(editingAccount.id, values)}
    />
  );
};

interface EditAccountDialogProps {
  account: AccountStats;
  open: boolean;
  onCancel: () => void;
  onSubmit: (values: EditFormShape) => Promise<void>;
}

const EditAccountDialog: React.FC<EditAccountDialogProps> = ({ account, open, onCancel, onSubmit }) => {
  const [form] = Form.useForm<EditFormShape>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');

  // 只在挂载时按初值填充一次，之后由用户输入驱动
  useEffect(() => {
    form.setFieldsValue(toFormValues(account));
  }, [form, account]);

  const handleOk = async () => {
    if (saving) return;
    let values: EditFormShape;
    try {
      values = await form.validateFields();
    } catch {
      return;
    }
    setSaving(true);
    setError('');
    try {
      await onSubmit(values);
    } catch (err: any) {
      setError(err?.message || '更新失败，请重试');
    } finally {
      setSaving(false);
    }
  };

  const handleCancel = () => {
    if (saving) return;
    onCancel();
  };

  return (
    <Modal
      title={`编辑 ${account.name}`}
      open={open}
      onOk={() => void handleOk()}
      onCancel={handleCancel}
      okText="保存并更新"
      cancelText="取消"
      confirmLoading={saving}
      closable={!saving}
      maskClosable={!saving}
      keyboard={!saving}
      cancelButtonProps={{ disabled: saving }}
      okButtonProps={{ disabled: saving }}
      width={520}
      destroyOnHidden
    >
      <Form form={form} layout="vertical" disabled={saving} requiredMark>
        <Row gutter={12}>
          <Col xs={24} sm={14}>
            <Form.Item
              name="name"
              label="账号展示名称"
              rules={[{ required: true, whitespace: true, message: '请输入账号名称' }]}
            >
              <Input placeholder="例如：团队主力账号" maxLength={64} />
            </Form.Item>
          </Col>
          <Col xs={24} sm={10}>
            <Form.Item name="plan" label="计划标记" extra="仅本地标记，不影响上游订阅">
              <Select
                options={[
                  { value: '', label: '自动识别' },
                  { value: 'plus', label: 'Plus' },
                  { value: 'prolite', label: 'Pro Lite' },
                  { value: 'pro', label: 'Pro' },
                  { value: 'team', label: 'Team' },
                  { value: 'enterprise', label: 'Enterprise' },
                  { value: 'free', label: 'Free' },
                ]}
              />
            </Form.Item>
          </Col>
        </Row>

        <Form.Item name="email" label="绑定邮箱" rules={[{ type: 'email', message: '邮箱格式不正确' }]}>
          <Input placeholder="user@example.com" autoComplete="off" />
        </Form.Item>

        <Form.Item
          name="max_concurrency"
          label="最大并发槽位（0 = 不限）"
          rules={[
            { required: true, message: '请输入并发槽位' },
            { type: 'integer', min: 0, message: '请输入非负整数' },
          ]}
        >
          <InputNumber min={0} max={64} precision={0} style={{ width: '100%' }} />
        </Form.Item>
      </Form>

      <Text type="secondary" style={{ fontSize: 12 }}>
        账号 ID <Text code style={{ fontSize: 12 }}>{account.id}</Text>
      </Text>

      {error && (
        <Alert
          type="error"
          showIcon
          title={`保存失败：${error}`}
          description="配置未改动，可修正后再次保存。"
          style={{ marginTop: 12 }}
        />
      )}
    </Modal>
  );
};
