import React from 'react';
import { Card, Skeleton, theme } from 'antd';

/** #rrggbb → rgba(r,g,b,a)：图标底色等需要同色系半透明的场景 */
function withAlpha(hex: string, alpha: number): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

interface StatCardProps {
  title: React.ReactNode;
  value: React.ReactNode;
  suffix?: React.ReactNode;
  icon: React.ReactNode;
  /** 强调色（十六进制），用于图标与图标底色 */
  color: string;
  footer?: React.ReactNode;
  loading?: boolean;
  /**
   * 紧凑单行版：窄屏下四张指标卡纵排会吃光视口高度，
   * 把标题压成一行、数值与图标并排，整卡高度降到约 56px。
   */
  compact?: boolean;
}

/** 指标卡：标题 + 图标徽记 + 大号数值 + 附注，统计页与账号页共用 */
export const StatCard: React.FC<StatCardProps> = ({ title, value, suffix, icon, color, footer, loading, compact }) => {
  const { token } = theme.useToken();

  const badge = (size: number, fontSize: number) => (
    <div
      style={{
        width: size,
        height: size,
        borderRadius: size <= 30 ? 8 : 10,
        display: 'grid',
        placeItems: 'center',
        fontSize,
        color,
        background: withAlpha(color, 0.12),
        flexShrink: 0,
      }}
    >
      {icon}
    </div>
  );

  if (compact) {
    return (
      <Card
        variant="outlined"
        style={{ height: '100%', boxShadow: token.boxShadowTertiary }}
        styles={{ body: { padding: '10px 12px' } }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          {badge(30, 15)}
          <div style={{ minWidth: 0, flex: 1 }}>
            <div
              style={{
                color: token.colorTextSecondary,
                fontSize: 12,
                lineHeight: 1.3,
                whiteSpace: 'nowrap',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
              }}
            >
              {title}
            </div>
            {loading ? (
              <Skeleton.Input active size="small" style={{ width: 56, height: 20, minWidth: 56 }} />
            ) : (
              <div
                style={{
                  fontSize: 19,
                  fontWeight: 600,
                  lineHeight: 1.25,
                  color: token.colorTextHeading,
                  fontVariantNumeric: 'tabular-nums',
                  whiteSpace: 'nowrap',
                }}
              >
                {value}
                {suffix != null && (
                  <span style={{ fontSize: 12, fontWeight: 500, color: token.colorTextTertiary, marginLeft: 3 }}>
                    {suffix}
                  </span>
                )}
              </div>
            )}
          </div>
        </div>
      </Card>
    );
  }

  return (
    <Card
      variant="outlined"
      style={{ height: '100%', boxShadow: token.boxShadowTertiary }}
      styles={{ body: { padding: '18px 20px' } }}
    >
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12 }}>
        <div style={{ minWidth: 0 }}>
          <div style={{ color: token.colorTextSecondary, fontSize: 13, marginBottom: 8 }}>{title}</div>
          {loading ? (
            <Skeleton.Input active size="small" style={{ width: 96 }} />
          ) : (
            <div
              style={{
                fontSize: 26,
                fontWeight: 600,
                lineHeight: 1.15,
                color: token.colorTextHeading,
                fontVariantNumeric: 'tabular-nums',
                whiteSpace: 'nowrap',
              }}
            >
              {value}
              {suffix != null && (
                <span style={{ fontSize: 13, fontWeight: 500, color: token.colorTextTertiary, marginLeft: 4 }}>
                  {suffix}
                </span>
              )}
            </div>
          )}
        </div>
        {badge(40, 18)}
      </div>
      {footer && (
        <div
          style={{
            marginTop: 12,
            paddingTop: 10,
            borderTop: `1px dashed ${token.colorBorderSecondary}`,
            fontSize: 12,
            color: token.colorTextTertiary,
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
          }}
        >
          {footer}
        </div>
      )}
    </Card>
  );
};
