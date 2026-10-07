import { Waypoints } from "lucide-react";

export default function BrandMark({ className = "" }: { className?: string }) {
  return (
    <span
      className={`grid size-8 flex-none place-items-center rounded-lg bg-primary text-primary-foreground ${className}`}
      aria-hidden
    >
      <Waypoints className="size-[18px]" />
    </span>
  );
}
