export default function Placeholder({
  title,
  desc,
}: {
  title: string
  desc: string
}) {
  return (
    <div>
      <h2>{title}</h2>
      <p className="subtitle">该页面为规划占位。</p>
      <div className="glass panel" style={{ padding: '28px' }}>
        <div style={{ fontSize: '15px', color: '#e2e8f0', lineHeight: 1.9 }}>{desc}</div>
      </div>
    </div>
  )
}
