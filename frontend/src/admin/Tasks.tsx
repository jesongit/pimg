import {ActivityPage} from "./Logs";
import SystemSchedules from "./SystemSchedules";

export default function Tasks(){return <div className="tasks-page"><ActivityPage record="tasks" activeOnly/><SystemSchedules/></div>}
