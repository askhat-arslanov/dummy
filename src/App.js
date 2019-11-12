import React from "react";
import "./App.css";
import { Route, Switch, Link } from "react-router-dom";

import Dummy from "./dummy";

function App() {
  return (
    <div className="App">
      <div>
        <Link to="/111">One</Link>
        <br />
        <Link to="/222">Two</Link>
        <br />
        <Link to="/333">Three</Link>
        <br />
      </div>
      <div>
        <Switch>
          <Route
            exact
            path="/111"
            component={() => (
              <Dummy
                title="One"
                text="OneOneOneOneOneOneOneOneOneOneOneOneOneOneOneOneOne"
              />
            )}
          />
          <Route
            exact
            path="/222"
            component={() => (
              <Dummy
                title="Two"
                text="TwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwoTwo"
              />
            )}
          />
          <Route
            exact
            path="/333"
            component={() => (
              <Dummy
                title="Three"
                text="ThreeThreeThreeThreeThreeThreeThreeThreeThreeThree"
              />
            )}
          />
          <Route
            exact
            to="/"
            component={() => <Dummy title="None" text="none" />}
          />
        </Switch>
      </div>
    </div>
  );
}

export default App;
