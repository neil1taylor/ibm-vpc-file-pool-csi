import React, { useEffect, useState } from "react";
import { useK8sWatchResource } from "@openshift-console/dynamic-plugin-sdk";
import { Link, useParams } from "react-router-dom";
import {
  Alert,
  Bullseye,
  Card,
  CardBody,
  CardTitle,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
  PageSection,
  Spinner,
  Split,
  SplitItem,
  Title,
} from "@patternfly/react-core";
import { Table, Thead, Tr, Th, Td, Tbody } from "@patternfly/react-table";
import { PoolBenchModel, VirtualMachineInstanceModel } from "../../models";
import { PoolBench } from "../../types";
import PhaseStatus from "../common/PhaseStatus";
import { ROUTES } from "../../constants";
import {
  bannerText,
  expectedEnd,
  isActive,
  ms,
  percentOfLimit,
  resultLabel,
  stepMarks,
  verdictText,
} from "../../utils/poolbench";
import { POOLBENCH_KEPT, POOLBENCH_SHARED_NODES } from "./poolbench-texts";
import RunCharts from "./RunCharts";
import { useVMNumbers } from "../../utils/use-run-metrics";

const OS_LABEL = "storage.ibmcloud.io/poolbench-os";
const RUN_LABEL = "storage.ibmcloud.io/poolbench";

interface VMI {
  metadata?: { name?: string; labels?: Record<string, string> };
  status?: { phase?: string; nodeName?: string };
}

const Term: React.FC<{ term: string; children: React.ReactNode }> = ({
  term,
  children,
}) => (
  <DescriptionListGroup>
    <DescriptionListTerm>{term}</DescriptionListTerm>
    <DescriptionListDescription>{children}</DescriptionListDescription>
  </DescriptionListGroup>
);

/** The current time, every 30 seconds, so the time left counts down between updates of the run. */
const useNow = (): Date => {
  const [now, setNow] = useState(new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 30000);
    return () => clearInterval(t);
  }, []);
  return now;
};

const windowsMix = (every?: number): string => {
  if (!every) return "all Linux";
  if (every === 1) return "all Windows";
  return `1 in ${every} Windows`;
};

const SettingsCard: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const s = pb.spec;
  const l = s.load || {};
  const k = s.knee || {};
  return (
    <Card data-testid="poolbench-settings">
      <CardTitle>Settings</CardTitle>
      <CardBody>
        <DescriptionList isCompact isHorizontal>
          <Term term="Share">
            {s.shareSizeGB} GB at {s.iops} IOPS
            {pb.status?.poolName ? ` (pool ${pb.status.poolName})` : ""}
          </Term>
          <Term term="VMs">
            {s.stepVMs} VMs per step, up to {s.maxVMs},{" "}
            {windowsMix(s.windowsEvery)}
          </Term>
          <Term term="Load per VM">
            {l.steadyIOPS} IOPS steady, {l.burstIOPS} IOPS bursts of{" "}
            {l.burstSeconds} s every {l.periodSeconds} s, {l.readPercent}%
            reads, blocks {l.blockMix}
          </Term>
          <Term term="Each step">
            {s.settleMinutes} min to settle, then {s.measureMinutes} min of
            measuring
          </Term>
          <Term term="Knee rule">
            p95 over {k.latencyLimitMs} ms on more than {k.slowVMPercent}% of
            the VMs, or a burst p95 over 50 ms on as many, or any I/O over{" "}
            {k.stallSeconds} s, or the share at 95% of its IOPS limit
            {k.stopAtKnee === false
              ? "; the run goes on after the knee"
              : "; the run stops at the knee"}
          </Term>
          {pb.status?.namespace && (
            <Term term="Namespace">{pb.status.namespace}</Term>
          )}
        </DescriptionList>
      </CardBody>
    </Card>
  );
};

const StepTable: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const steps = pb.status?.steps || [];
  if (steps.length === 0) return <p>No step has started yet.</p>;
  return (
    <Table aria-label="Step results" variant="compact" role="grid">
      <Thead>
        <Tr>
          <Th>VMs</Th>
          <Th>Share IOPS</Th>
          <Th>Guest IOPS</Th>
          <Th>IBM operations (of limit)</Th>
          <Th>Share latency</Th>
          <Th>p50</Th>
          <Th>p95</Th>
          <Th>p99</Th>
          <Th>Slow VMs</Th>
          <Th>Stalls</Th>
          <Th>Ready after</Th>
          <Th>Result</Th>
        </Tr>
      </Thead>
      <Tbody>
        {steps.map((s, i) => {
          const done = !!s.ended;
          return (
            <Tr key={`${s.vms}-${i}`}>
              <Td dataLabel="VMs">
                {s.vms} ({s.windowsVMs ?? 0} Windows)
              </Td>
              <Td dataLabel="Share IOPS">{done ? (s.shareIOPS ?? 0) : "-"}</Td>
              <Td dataLabel="Guest IOPS">{done ? (s.guestIOPS ?? 0) : "-"}</Td>
              <Td dataLabel="IBM operations">
                {done
                  ? `${s.ibmOps ?? 0} (${percentOfLimit(s.ibmOps, pb.spec.iops)})`
                  : "-"}
              </Td>
              <Td dataLabel="Share latency">
                {done ? ms(s.shareLatencyMicros) : "-"}
              </Td>
              <Td dataLabel="p50">{done ? ms(s.p50Micros) : "-"}</Td>
              <Td dataLabel="p95">{done ? ms(s.p95Micros) : "-"}</Td>
              <Td dataLabel="p99">{done ? ms(s.p99Micros) : "-"}</Td>
              <Td dataLabel="Slow VMs">
                {done ? `${s.slowPercent ?? 0}%` : "-"}
              </Td>
              <Td dataLabel="Stalls">{done ? (s.stalls ?? 0) : "-"}</Td>
              <Td dataLabel="Ready after">
                {s.readySeconds
                  ? `${Math.round(s.readySeconds / 60)} min`
                  : "-"}
              </Td>
              <Td dataLabel="Result">
                {done || !isActive(pb) ? (
                  <Label color={s.pass ? "green" : "red"}>
                    {resultLabel(s)}
                  </Label>
                ) : (
                  <Label color="blue">In progress</Label>
                )}{" "}
                {stepMarks(pb, s.vms).map((m) => (
                  <Label key={m} color={m === "Knee" ? "orange" : "purple"}>
                    {m}
                  </Label>
                ))}
              </Td>
            </Tr>
          );
        })}
      </Tbody>
    </Table>
  );
};

const VMTable: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const ns = pb.status?.namespace;
  const [vmis] = useK8sWatchResource<VMI[]>(
    ns && isActive(pb)
      ? {
          groupVersionKind: {
            group: VirtualMachineInstanceModel.apiGroup!,
            version: VirtualMachineInstanceModel.apiVersion,
            kind: VirtualMachineInstanceModel.kind,
          },
          namespace: ns,
          isList: true,
          selector: { matchLabels: { [RUN_LABEL]: pb.metadata?.name || "" } },
        }
      : null,
  );
  const numbers = useVMNumbers(pb.metadata?.name || "", !!ns && isActive(pb));
  const list = [...(vmis || [])].sort((a, b) =>
    (a.metadata?.name || "").localeCompare(b.metadata?.name || ""),
  );
  if (list.length === 0) return <p>No test VM is running.</p>;
  return (
    <Table aria-label="Test VMs" variant="compact" role="grid">
      <Thead>
        <Tr>
          <Th>Name</Th>
          <Th>OS</Th>
          <Th>Node</Th>
          <Th>State</Th>
          <Th>Last IOPS</Th>
          <Th>Last p95</Th>
        </Tr>
      </Thead>
      <Tbody>
        {list.map((v) => {
          const n = numbers[v.metadata?.name || ""];
          return (
            <Tr key={v.metadata?.name}>
              <Td dataLabel="Name">{v.metadata?.name}</Td>
              <Td dataLabel="OS">
                {v.metadata?.labels?.[OS_LABEL] === "windows"
                  ? "Windows"
                  : "Linux"}
              </Td>
              <Td dataLabel="Node">{v.status?.nodeName || "-"}</Td>
              <Td dataLabel="State">{v.status?.phase || "-"}</Td>
              <Td dataLabel="Last IOPS">
                {n && Number.isFinite(n.iops) ? n.iops : "-"}
              </Td>
              <Td dataLabel="Last p95">
                {n && Number.isFinite(n.p95Ms) ? `${n.p95Ms} ms` : "-"}
              </Td>
            </Tr>
          );
        })}
      </Tbody>
    </Table>
  );
};

const LOG_COLORS: Record<string, "blue" | "orange" | "red" | "purple"> = {
  Info: "blue",
  Warning: "orange",
  Error: "red",
  Control: "purple",
};

const RunLog: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const log = [...(pb.status?.log || [])].reverse();
  return (
    <div data-testid="poolbench-log">
      {log.length === 0 && <p>Nothing logged yet.</p>}
      {log.map((e, i) => (
        <div key={i} style={{ marginBottom: 4 }}>
          <span style={{ color: "var(--pf-t--global--color--subtle)" }}>
            {new Date(e.time).toLocaleString()}
          </span>{" "}
          <Label isCompact color={LOG_COLORS[e.kind] || "blue"}>
            {e.kind}
          </Label>{" "}
          {e.message}
        </div>
      ))}
    </div>
  );
};

const PoolBenchRunPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const now = useNow();
  const [pb, loaded, loadError] = useK8sWatchResource<PoolBench>({
    groupVersionKind: {
      group: PoolBenchModel.apiGroup,
      version: PoolBenchModel.apiVersion,
      kind: PoolBenchModel.kind,
    },
    name,
  });

  if (!loaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }
  if (loadError || !pb) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading the run" isInline>
          {loadError?.message || "The run was not found."}
        </Alert>
      </PageSection>
    );
  }
  const st = pb.status || {};
  const end = expectedEnd(pb, now);
  const verdict = verdictText(pb);
  const kept =
    !isActive(pb) &&
    pb.spec.keepAfterEnd &&
    !st.cleanedUp &&
    st.phase !== "Failed";

  return (
    <PageSection>
      <Split hasGutter style={{ marginBottom: 16 }}>
        <SplitItem isFilled>
          <Title headingLevel="h1">
            <Link to={ROUTES.POOLBENCH}>PoolBench</Link> / {name}
          </Title>
        </SplitItem>
        <SplitItem>
          <PhaseStatus phase={st.phase || "Pending"} />
        </SplitItem>
      </Split>

      <Alert
        variant={
          st.phase === "Failed"
            ? "danger"
            : st.phase === "Paused"
              ? "warning"
              : isActive(pb)
                ? "info"
                : "success"
        }
        isInline
        title={bannerText(pb, now)}
        style={{ marginBottom: 16 }}
      >
        {st.startTime && (
          <>Started {new Date(st.startTime).toLocaleString()}. </>
        )}
        {end && (
          <>
            Expected end about {end.toLocaleTimeString()} if the run goes to its
            last step.{" "}
          </>
        )}
        {isActive(pb) && POOLBENCH_SHARED_NODES}
      </Alert>
      {kept && (
        <Alert
          variant="warning"
          isInline
          title={POOLBENCH_KEPT}
          style={{ marginBottom: 16 }}
        />
      )}
      {verdict && (
        <Alert
          variant={st.kneeVMs ? "warning" : "success"}
          isInline
          title={verdict}
          style={{ marginBottom: 16 }}
        />
      )}

      <div style={{ marginBottom: 16 }}>
        <RunCharts pb={pb} />
      </div>

      <Card style={{ marginBottom: 16 }}>
        <CardTitle>Step results</CardTitle>
        <CardBody>
          <StepTable pb={pb} />
        </CardBody>
      </Card>

      <div style={{ marginBottom: 16 }}>
        <SettingsCard pb={pb} />
      </div>

      <Card style={{ marginBottom: 16 }}>
        <CardTitle>Test VMs</CardTitle>
        <CardBody>
          <VMTable pb={pb} />
        </CardBody>
      </Card>

      <Card>
        <CardTitle>Run log</CardTitle>
        <CardBody>
          <RunLog pb={pb} />
        </CardBody>
      </Card>
    </PageSection>
  );
};

export default PoolBenchRunPage;
