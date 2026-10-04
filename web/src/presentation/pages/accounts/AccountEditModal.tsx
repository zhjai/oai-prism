import React, { useEffect } from 'react';
import { Modal, Form, Input, Select, InputNumber, message } from 'antd';
import { useAccountStore } from '../../../application/account/store';

export const AccountEditModal: React.FC = () => {
  const { editingAccount, editModalOpen, closeEditModal, updateAccount } = useAccountStore();
  const [form] = Form.useForm();

  useEffect(() => {
    if (editingAccount) {
      form.setFieldsValue({
        name: editingAccount.name,
        email: editingAccount.email,
        plan: editingAccount.plan || 'pro',
        max_concurrency: editingAccount.max_concurrency ?? 2,
      });
    }
  }, [editingAccount, form]);

  const handleOk = async () => {
    try {
      const values = await form.validateFields();
      if (!editingAccount) return;
      await updateAccount(editingAccount.id, values);
      message.success(`账号 [${editingAccount.name}] 配置已持久化更新！`);
      closeEditModal();
    } catch (err: any) {
      if (err.errorFields) return;
      message.error(err.message || '更新失败');
    }
  };

  return (
    <Modal
      title={`编辑账号配置 - ${editingAccount?.id}`}
      open={editModalOpen}
      onOk={handleOk}
      onCancel={closeEditModal}
      okText="保存并更新"
      cancelText="取消"
      destroyOnClose
    >
      <Form form={form} layout="vertical">
        <Form.Item
          name="name"
          label="账号展示名称"
          rules={[{ required: true, message: '请输入账号名称' }]}
        >
          <Input placeholder="例如：团队主力账号" />
        </Form.Item>

        <Form.Item name="email" label="绑定邮箱">
          <Input placeholder="user@example.com" />
        </Form.Item>

        <Form.Item name="plan" label="计划等级（本地标记）" extra="不改变上游订阅或额度">
          <Select
            options={[
              { value: 'plus', label: 'Plus 计划' },
              { value: 'prolite', label: 'Pro Lite 计划' },
              { value: 'pro', label: 'Pro 计划' },
              { value: 'team', label: 'Team 计划' },
              { value: 'enterprise', label: 'Enterprise 计划' },
              { value: 'free', label: 'Free 计划' },
            ]}
          />
        </Form.Item>

        <Form.Item
          name="max_concurrency"
          label="最大并发槽位 (0 = 不限)"
          extra="设置允许同时在上游执行推理的请求数，超出将在网关排队调度"
        >
          <InputNumber min={0} max={64} style={{ width: '100%' }} />
        </Form.Item>
      </Form>
    </Modal>
  );
};
